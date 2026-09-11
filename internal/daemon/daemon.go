// Package daemon runs the two long-lived shome roles: the controller (job
// store, scheduler, node registry, APIs) and the node agent (runs and
// supervises jobs on one machine).
//
// Both live here rather than in their own commands so that a single `shome`
// binary can be a controller, an agent, or a client. That matters for setup:
// one binary to copy to a new machine, one command to run there, and no way to
// end up with a controller and an agent built from different revisions.
package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/davidwu/shome/internal/agent"
	"github.com/davidwu/shome/internal/agentapi"
	"github.com/davidwu/shome/internal/ctl"
	"github.com/davidwu/shome/internal/loginnode"
	"github.com/davidwu/shome/internal/owner"
	"github.com/davidwu/shome/internal/ownerweb"
	"github.com/davidwu/shome/internal/pki"
	"github.com/davidwu/shome/internal/sshca"
	"github.com/davidwu/shome/internal/store"
	"github.com/davidwu/shome/internal/userenv"
)

// DefaultPort is the port node agents dial. The bootstrap HTTP service that
// hands out binaries and join instructions sits on the next port up.
const DefaultPort = 7817

// ControllerConfig is everything the controller needs. Every field has a
// working default, so the common case is an empty struct plus a root.
type ControllerConfig struct {
	Root        string
	Listen      string // default 0.0.0.0:7817
	Node        string // name for this machine's local agent; default hostname
	LocalAgent  bool
	Advertise   string // extra names/IPs for the TLS certificate, comma-separated
	WebAddr     string // admin console address; empty disables it
	SSHAddr     string // login node address; empty disables it
	MonitorAddr string // owner's local page; empty disables it
	Bootstrap   bool   // serve the join bootstrap service
	Log         *slog.Logger
}

// AgentConfig is everything a node agent needs.
type AgentConfig struct {
	Root        string
	MonitorAddr string // owner's local page; empty disables it
	Controller  string // host:port (required)
	Token       string // join token, first run only
	Node        string // default hostname
	ServerName  string // expected certificate name; default the host part of Controller
	Log         *slog.Logger
}

// Hostname is this machine's short name, used as the default node name.
func Hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "localhost"
	}
	// A node name is a path component in scratch directories and appears in
	// every listing, so prefer "mini" over "mini.lan".
	if i := strings.Index(h, "."); i > 0 {
		h = h[:i]
	}
	return h
}

// HostPart strips the port from host:port.
func HostPart(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}

// RunController starts the controller and blocks until ctx is cancelled.
func RunController(ctx context.Context, cfg ControllerConfig) error {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	if cfg.Root == "" {
		cfg.Root = ctl.DefaultRoot()
	}
	if cfg.Listen == "" {
		cfg.Listen = fmt.Sprintf("0.0.0.0:%d", DefaultPort)
	}
	if cfg.Node == "" {
		cfg.Node = Hostname()
	}

	if err := os.MkdirAll(cfg.Root, 0o700); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	st, err := store.Open(filepath.Join(cfg.Root, "shome.db"))
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	ca, err := pki.LoadOrCreateCA(filepath.Join(cfg.Root, "pki"))
	if err != nil {
		return fmt.Errorf("certificate authority: %w", err)
	}

	// The owner's own credential. Reissued if the token file has gone missing,
	// because otherwise losing one file locks the owner out of their own
	// cluster permanently -- the token is stored hashed and cannot be reread.
	if created, err := ctl.EnsureAdminToken(ctx, st, cfg.Root); err != nil {
		return fmt.Errorf("admin credential: %w", err)
	} else if created {
		log.Info("wrote owner API token", "file", ctl.AdminTokenPath(cfg.Root))
	}

	c := ctl.New(st, cfg.Root, log)

	// SSH login-node support: two CAs (host and user) plus a host key signed
	// by the host CA, so clients never see a TOFU prompt.
	if err := setupSSH(c, cfg.Root, log); err != nil {
		log.Warn("SSH login node unavailable", "err", err)
	}

	// Bind the TCP agent listener FIRST, before touching the unix socket.
	//
	// Ordering matters here for a non-obvious reason: starting a second
	// controller used to take over the existing one's unix socket and only
	// then discover the TCP port was busy -- leaving the first controller
	// running but unreachable, with no way to shut it down cleanly. Acquiring
	// the contended resource first means a port clash fails before anything
	// shared has been disturbed.
	//
	// The certificate must cover every address an agent might dial. Without
	// the machine's LAN addresses, a remote agent pointed at 192.168.x.y
	// fails certificate verification with an error that looks like a
	// networking problem -- which is a bad afternoon for whoever hits it.
	hosts := CertHosts(cfg.Advertise)
	log.Info("controller certificate covers", "names", hosts)
	agSrv, agLn, err := ctl.ServeAgents(c, ca, cfg.Listen, hosts)
	if err != nil {
		return fmt.Errorf("agent api: %w", err)
	}
	go func() {
		if err := agSrv.Serve(agLn); err != nil && err != http.ErrServerClosed {
			log.Error("agent api", "err", err)
		}
	}()
	defer agSrv.Close()

	// Client API (unix socket, owner-only).
	cliSrv, cliLn, err := ctl.Serve(c, ctl.SocketPath(cfg.Root))
	if err != nil {
		return fmt.Errorf("client api: %w", err)
	}
	go func() {
		if err := cliSrv.Serve(cliLn); err != nil && err != http.ErrServerClosed {
			log.Error("client api", "err", err)
		}
	}()
	defer cliSrv.Close()

	if cfg.WebAddr != "" {
		wsrv, wln, werr := ctl.ServeConsole(c, cfg.WebAddr)
		if werr != nil {
			return fmt.Errorf("console listener: %w", werr)
		}
		go func() {
			if err := wsrv.Serve(wln); err != nil && err != http.ErrServerClosed {
				log.Error("console server", "err", err)
			}
		}()
		defer wsrv.Close()
		log.Info("admin console listening", "addr", cfg.WebAddr)
	}

	// Left undecided here on purpose. A definite answer -- listening, or
	// off, or broken and why -- is set below once the login node has
	// actually tried, and the zero LoginInfo means "not yet", so a caller
	// that asks during startup can tell that apart from "off" and wait.
	if cfg.SSHAddr == "" {
		c.SetLoginInfo(ctl.LoginInfo{Reason: "not configured on this machine"})
	}

	// The login node: how people who are not sitting at this machine use the
	// cluster. Started here so it is supervised with everything else rather
	// than being a separate daemon somebody has to remember.
	if cfg.SSHAddr != "" {
		// The login node's own backend, rooted where user files live rather
		// than in a job scratch tree: an interactive session's writable area
		// is the account's persistent directory.
		loginBE, beErr := newBackendAt(filepath.Join(cfg.Root, "login"), cfg.Root)
		if beErr != nil {
			log.Warn("interactive sessions unavailable on this machine", "err", beErr)
		}
		// Prepare the managed environment now, so the first person to log in
		// does not wait for it and an admin sees any problem at startup.
		if self, err := os.Executable(); err == nil {
			if err := userenv.EnsureManaged(cfg.Root, self); err != nil {
				log.Warn("could not prepare the session environment", "err", err)
			}
		}
		if !userenv.Ready(cfg.Root) {
			log.Warn("logged-in accounts will have no package manager; " +
				"run 'shome admin env install' to give them uv")
		}
		// The controller is a machine too, and on Linux its own jobs use
		// its own software.
		go userenv.Provision(ctx, cfg.Root, func(msg string, args ...any) {
			log.Info(msg, args...)
		})
		lsrv, lerr := loginnode.Listen(loginnode.Config{
			Addr:          cfg.SSHAddr,
			Root:          cfg.Root,
			Store:         st,
			UserCA:        c.UserCA(),
			Runner:        &loginnode.CLIRunner{Root: cfg.Root},
			Log:           log,
			Cluster:       cfg.Node,
			ClusterName:   c.ClusterName,
			NodeLabel:     "login",
			Backend:       loginBE,
			ControlSocket: ctl.SocketPath(cfg.Root),
		})
		if lerr != nil {
			// Not fatal: the cluster works, only remote access is lost. Said
			// out loud rather than left in the log, because the address
			// would otherwise be handed out as this cluster's while
			// something else answered on it.
			log.Warn("login node unavailable", "err", lerr)
			c.SetLoginInfo(ctl.LoginInfo{Reason: lerr.Error()})
		} else {
			// Let revocation reach sessions that are already open.
			c.SetRevocationHook(func(user, fingerprint string) {
				lsrv.CloseSessionsFor(user, fingerprint)
			})
			go func() {
				if err := lsrv.Serve(ctx); err != nil {
					log.Error("login node", "err", err)
				}
			}()
			defer lsrv.Close()
			// Where the login node really is, so the enrollment
			// instructions -- printed by the CLI and shown by the console --
			// name a real address instead of a configured intention.
			c.SetLoginInfo(loginInfo(cfg, lsrv.Addr()))
			log.Info("login node listening", "addr", lsrv.Addr())
		}
	}

	if cfg.Bootstrap {
		bsrv, bln, berr := ctl.ServeBootstrap(st, cfg.Root, BootstrapAddr(cfg.Listen), log)
		if berr != nil {
			// Not fatal: the cluster works, only one-command join is lost.
			log.Warn("join bootstrap service unavailable", "err", berr)
		} else {
			go func() {
				if err := bsrv.Serve(bln); err != nil && err != http.ErrServerClosed {
					log.Error("bootstrap server", "err", err)
				}
			}()
			defer bsrv.Close()
			log.Info("join bootstrap listening", "addr", bln.Addr().String())
		}
	}

	defer serveOwnerMonitor(ctx, cfg.Root, cfg.MonitorAddr, log)()

	// Record where this controller lives so the CLI finds it from any shell.
	ctl.WriteControllerPointer(cfg.Root)
	defer ctl.ClearControllerPointer(cfg.Root)

	log.Info("controller ready", "socket", ctl.SocketPath(cfg.Root),
		"agents", agLn.Addr().String(), "root", cfg.Root)

	if cfg.LocalAgent {
		ag, err := startLocalAgent(ctx, cfg.Root, agLn.Addr().String(), cfg.Node, st, c, log)
		if ag != nil {
			// On the way down, and synchronously: a container outlives the
			// process that started it, so leaving this to a goroutine
			// reacting to the cancelled context lost the race with the
			// process exiting -- and the virtual machine stayed up.
			defer ag.ReleaseIsolation()
		}
		if err != nil {
			log.Error("local agent did not start", "err", err)
		}
	}

	err = c.Run(ctx)
	log.Info("controller stopped")
	if err == context.Canceled {
		return nil
	}
	return err
}

// BootstrapAddr is the plain-HTTP bootstrap address derived from the agent
// listen address: same interface, one port up.
func BootstrapAddr(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Sprintf("0.0.0.0:%d", DefaultPort+1)
	}
	p := DefaultPort
	fmt.Sscanf(port, "%d", &p)
	return net.JoinHostPort(host, fmt.Sprint(p+1))
}

// RunAgent joins this machine to a controller if needed, then runs jobs for it
// until ctx is cancelled.
func RunAgent(ctx context.Context, cfg AgentConfig) error {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	if cfg.Controller == "" {
		return fmt.Errorf("controller address is required")
	}
	if cfg.Root == "" {
		cfg.Root = ctl.DefaultRoot()
	}
	if cfg.Node == "" {
		cfg.Node = Hostname()
	}
	if cfg.ServerName == "" {
		cfg.ServerName = HostPart(cfg.Controller)
	}

	if err := os.MkdirAll(cfg.Root, 0o700); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}

	dir := ctl.AgentStatePath(cfg.Root)
	caPEM, certPEM, keyPEM, err := ctl.LoadAgentIdentity(dir)
	if err != nil {
		if cfg.Token == "" {
			return fmt.Errorf("this machine has no cluster identity and no join token was given;\n"+
				"run  shome invite %s  on the controller and follow what it prints", cfg.Node)
		}
		log.Info("joining cluster", "controller", cfg.Controller, "node", cfg.Node)
		jr, err := agent.Join(ctx, cfg.Controller, cfg.Node, cfg.Token)
		if err != nil {
			return err
		}
		if err := ctl.SaveAgentIdentity(dir, jr); err != nil {
			return err
		}
		caPEM, certPEM, keyPEM = []byte(jr.CACert), []byte(jr.NodeCert), []byte(jr.NodeKey)
		log.Info("joined; identity saved", "dir", dir)
	} else if cfg.Token != "" {
		log.Info("already a cluster member; ignoring the join token", "dir", dir)
	}

	// Remember how to reconnect, so `shome up` needs no arguments next time.
	SaveAgentConfig(cfg.Root, cfg)

	// Record this agent's root so owner commands (`shome status`, `pause`,
	// `resume`) work from any shell on this machine without SHOME_ROOT.
	ctl.WriteNodePointer(cfg.Root)

	be, err := newBackend(cfg.Root)
	if err != nil {
		return err
	}
	caps, err := be.Inventory(ctx)
	if err != nil {
		return fmt.Errorf("inventory: %w", err)
	}
	log.Info("node inventory", "node", cfg.Node, "os", caps.OS, "arch", caps.Arch,
		"tier", caps.Tier, "cpus", caps.CPUs, "mem_mib", caps.MemBytes>>20,
		"mem_enforcement", caps.MemLimit, "cpu_enforcement", caps.CPULimit)
	// Degrade loudly, every startup.
	for _, l := range caps.Lost {
		log.Warn("limitation", "detail", l)
	}

	// shome's own commands, where jobs on this machine will look for them.
	// The controller does this for its login node; a node needs it for the
	// same reason one directory down -- a job here has the tool directory
	// on its PATH, and on a machine that only ever ran jobs nothing had
	// ever put anything in it.
	if self, err := os.Executable(); err == nil {
		if err := userenv.EnsureManaged(cfg.Root, self); err != nil {
			log.Warn("the cluster commands will be missing inside jobs here", "err", err)
		}
	}

	// And the software the cluster's jobs expect. In the background: a
	// package manager can take minutes, and a node that has most of what
	// it needs should be running work while the rest arrives rather than
	// refusing to start. See userenv.Provision for what it will and will
	// not do to a machine.
	go userenv.Provision(ctx, cfg.Root, func(msg string, args ...any) {
		log.Info(msg, args...)
	})

	ag := agent.New(be, cfg.Node, cfg.Root, log)
	// On the way down, and synchronously: a container outlives the process
	// that started it, so leaving this to a goroutine reacting to the
	// cancelled context lost the race with the process exiting -- and the
	// virtual machine stayed up with nobody to stop it.
	defer ag.ReleaseIsolation()
	// The machine owner's policy governs this node; an admin cannot override it.
	ag.Owner = owner.NewManager(cfg.Root, owner.DarwinSensor{})
	// Nothing is running yet, so every job directory found now is left over
	// from a previous run that did not get to tidy up.
	ag.SweepOrphanScratch(ctx)
	cli, err := agent.NewClient(cfg.Controller, cfg.Node, caPEM, certPEM, keyPEM, cfg.ServerName)
	if err != nil {
		return err
	}
	cli.StatusRoot = cfg.Root
	// The relay for interactive jobs -- srun's live output, and srun --pty.
	ag.Relay = cli
	// And the same connection carries a job's own cluster-API requests, so
	// `squeue` works from inside one. See internal/ctl/jobapi.go.
	ag.API = cli

	// The cluster commands, for whoever is sitting at this machine.
	//
	// A node has no controller of its own, so `shome squeue` here used to
	// report that shomectld was not running -- on a machine that had joined
	// a working cluster. The same forwarding a job gets serves the owner's
	// shell: the socket is where the CLI already looks, and what goes over
	// it still needs that person's own token.
	if psrv, pln, perr := ctl.ServeClientProxy(ctl.SocketPath(cfg.Root),
		http.HandlerFunc(cli.ProxyAPI)); perr != nil {
		log.Warn("the cluster commands will not work on this machine", "err", perr)
	} else {
		go func() {
			if err := psrv.Serve(pln); err != nil && err != http.ErrServerClosed {
				log.Error("client api", "err", err)
			}
		}()
		defer psrv.Close()
	}
	defer serveOwnerMonitor(ctx, cfg.Root, cfg.MonitorAddr, log)()

	go ag.Supervise(ctx)
	log.Info("agent ready", "controller", cfg.Controller, "node", cfg.Node)
	ag.RunLoop(ctx, cli, func() agentapi.Heartbeat { return agentapi.Heartbeat{Caps: caps} })
	log.Info("agent stopped")
	return nil
}

// startLocalAgent joins this machine to its own controller. It mints a token
// and redeems it immediately, so the local agent uses exactly the same path a
// remote one would.
func startLocalAgent(ctx context.Context, root, addr, node string, st *store.Store,
	c *ctl.Controller, log *slog.Logger) (*agent.Agent, error) {
	dir := ctl.AgentStatePath(filepath.Join(root, "local"))
	caPEM, certPEM, keyPEM, err := ctl.LoadAgentIdentity(dir)
	if err != nil {
		tok, err := st.CreateJoinToken(ctx, ctl.JoinTokenTTL, time.Now())
		if err != nil {
			return nil, err
		}
		jr, err := agent.Join(ctx, addr, node, tok)
		if err != nil {
			return nil, err
		}
		if err := ctl.SaveAgentIdentity(dir, jr); err != nil {
			return nil, err
		}
		caPEM, certPEM, keyPEM = []byte(jr.CACert), []byte(jr.NodeCert), []byte(jr.NodeKey)
		log.Info("local agent joined", "node", node)
	}

	// The local agent's scratch lives under root/local, but its disk
	// accounting covers the whole installation.
	be, err := newBackendAt(filepath.Join(root, "local"), root)
	if err != nil {
		return nil, err
	}
	caps, err := be.Inventory(ctx)
	if err != nil {
		return nil, err
	}
	log.Info("local node inventory", "node", node, "os", caps.OS, "arch", caps.Arch,
		"tier", caps.Tier, "cpus", caps.CPUs, "mem_mib", caps.MemBytes>>20,
		"mem_enforcement", caps.MemLimit, "cpu_enforcement", caps.CPULimit)
	for _, l := range caps.Lost {
		log.Warn("limitation", "node", node, "detail", l)
	}

	// The controller's own agent keeps user files under the installation
	// root, alongside where the controller keeps everything else.
	ag := agent.New(be, node, root, log)
	// The machine owner's policy governs this node; an admin cannot override it.
	ag.Owner = owner.NewManager(root, owner.DarwinSensor{})
	ag.SweepOrphanScratch(ctx)
	cli, err := agent.NewClient(addr, node, caPEM, certPEM, keyPEM, "localhost")
	if err != nil {
		return nil, err
	}
	// The controller's own agent publishes to the installation root, which is
	// where the owner's tools look.
	cli.StatusRoot = root
	ag.Relay = cli
	ag.API = cli
	// `shome storage put` writes into this machine's per-user area behind
	// the agent's back, so tell the agent to re-measure rather than leave
	// the file missing from `shome fs` until the next scan.
	c.Storage().SetChangeHook(func(string) { ag.InvalidateStorage() })
	go ag.Supervise(ctx)
	go ag.RunLoop(ctx, cli, func() agentapi.Heartbeat {
		return agentapi.Heartbeat{Caps: caps}
	})
	return ag, nil
}

// setupSSH prepares the login node's certificate material. Idempotent.
func setupSSH(c *ctl.Controller, root string, log *slog.Logger) error {
	dir := filepath.Join(root, "ssh")
	userCA, err := sshca.LoadOrCreate(dir, "user")
	if err != nil {
		return err
	}
	hostCA, err := sshca.LoadOrCreate(dir, "host")
	if err != nil {
		return err
	}
	c.SetUserCA(userCA)
	// The account sshd will run sessions as. shome users are not OS users, so
	// they all log in through this one and are told apart by the certificate.
	login := os.Getenv("USER")
	if login == "" {
		login = "shome"
	}
	c.SetLoginAccounts([]string{login})

	hostKey := filepath.Join(dir, "login_host_key")
	if _, err := os.Stat(hostKey); err != nil {
		cmd := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "shome-login", "-f", hostKey)
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("generate host key: %w", err)
		}
	}
	pub, err := os.ReadFile(hostKey + ".pub")
	if err != nil {
		return err
	}
	certPath := hostKey + "-cert.pub"
	if _, err := os.Stat(certPath); err != nil {
		h := Hostname()
		cert, err := hostCA.SignHostKey(pub, []string{"localhost", "127.0.0.1", h, h + ".local"}, 10*365*24*time.Hour)
		if err != nil {
			return err
		}
		if err := os.WriteFile(certPath, cert, 0o644); err != nil {
			return err
		}
	}
	log.Info("ssh login node ready", "user_ca", userCA.PublicKeyPath(), "host_cert", certPath)
	return nil
}

// CertHosts is every name and address an agent could reasonably use to reach
// this controller: loopback, the hostname, and each non-loopback interface
// address. extra is an operator-supplied name for cases this cannot guess,
// such as a DNS name or a VPN address assigned later.
func CertHosts(extra string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(h string) {
		if h == "" || seen[h] {
			return
		}
		seen[h] = true
		out = append(out, h)
	}
	add("localhost")
	add("127.0.0.1")
	add("::1")
	full, _ := os.Hostname()
	add(full)
	// full may already be fully qualified; do not produce "host.local.local".
	if !strings.Contains(full, ".") {
		add(full + ".local")
	}
	add(Hostname())
	for _, p := range strings.Split(extra, ",") {
		add(strings.TrimSpace(p))
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return out
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() || ipnet.IP.IsLinkLocalUnicast() {
			continue
		}
		add(ipnet.IP.String())
	}
	return out
}

// LANAddress is the address other machines on this network should dial to
// reach us. Loopback is useless to anyone else, so it is never returned.
func LANAddress() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	var v6 string
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() || ipnet.IP.IsLinkLocalUnicast() {
			continue
		}
		if ip4 := ipnet.IP.To4(); ip4 != nil {
			return ip4.String()
		}
		if v6 == "" {
			v6 = ipnet.IP.String()
		}
	}
	return v6
}

// LANAddresses returns every address another machine could reach this one
// on, IPv4 first.
//
// All of them rather than one, because a machine with Wi-Fi and Ethernet and
// a VPN has several and only the person reading the instructions knows which
// network the other machine is on. Handing out an arbitrary one is how
// `shome invite` once printed an address that could not be reached.
func LANAddresses() []string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var v4, v6 []string
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() || ipnet.IP.IsLinkLocalUnicast() {
			continue
		}
		if ip4 := ipnet.IP.To4(); ip4 != nil {
			v4 = append(v4, ip4.String())
			continue
		}
		v6 = append(v6, ipnet.IP.String())
	}
	return append(v4, v6...)
}

// loginInfo works out where an outside user reaches this cluster.
//
// The configured advertise addresses come first, because an admin who set
// them did so precisely to say which address to hand out -- typically a VPN
// address that the interface list would bury behind a LAN one.
func loginInfo(cfg ControllerConfig, bound string) ctl.LoginInfo {
	out := ctl.LoginInfo{Enabled: true, Port: 2222}
	// The port it is actually listening on, which is not always the one
	// asked for: a request for port 0 means "any free one".
	for _, a := range []string{bound, cfg.SSHAddr} {
		if a == "" {
			continue
		}
		if _, p, err := net.SplitHostPort(a); err == nil {
			if n, err := strconv.Atoi(p); err == nil && n > 0 {
				out.Port = n
				break
			}
		}
	}
	seen := map[string]bool{}
	add := func(h string) {
		if h = strings.TrimSpace(h); h != "" && !seen[h] {
			seen[h] = true
			out.Hosts = append(out.Hosts, h)
		}
	}
	for _, a := range strings.Split(cfg.Advertise, ",") {
		// An advertise entry may carry a port for the agent API, which is
		// not the login node's port.
		if h, _, err := net.SplitHostPort(a); err == nil {
			add(h)
			continue
		}
		add(a)
	}
	for _, a := range LANAddresses() {
		add(a)
	}
	return out
}

// serveOwnerMonitor starts the machine owner's local page, if enabled.
//
// Started for both roles: every machine has an owner, and the page reads the
// status file the agent writes locally, so it works on a node with no
// controller in sight. Not fatal if it cannot start -- losing a convenience
// must not stop the machine doing work.
func serveOwnerMonitor(ctx context.Context, root, addr string, log *slog.Logger) func() {
	if addr == "" {
		return func() {}
	}
	srv, ln, err := ownerweb.Listen(root, addr, log)
	if err != nil {
		log.Warn("owner monitor page unavailable", "err", err)
		return func() {}
	}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Error("owner monitor page", "err", err)
		}
	}()
	log.Info("owner monitor listening", "addr", ln.Addr().String())
	return func() { srv.Close() }
}
