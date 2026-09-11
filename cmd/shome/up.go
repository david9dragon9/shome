package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/davidwu/shome/internal/ctl"
	"github.com/davidwu/shome/internal/daemon"
)

// The commands in this file are the whole lifecycle: up, down, restart, logs.
// They exist because the alternative was asking people to background a daemon
// with '&', pass flags that had to agree with flags on another machine, and
// find the process by hand to stop it. A cluster you cannot confidently stop
// is a cluster you do not want to start.

// defaultWebAddr is the admin console. Loopback only: it is an unauthenticated
// convenience surface, and binding it to the network would hand the cluster to
// anyone on the LAN.
const defaultWebAddr = "127.0.0.1:7820"

// defaultSSHAddr is the login node. Bound to every interface, unlike the web
// console, because reaching it from another machine is its entire purpose --
// and unlike the console it authenticates every connection with a public key
// against the account database before anything else happens.
const defaultSSHAddr = "0.0.0.0:2222"

// defaultMonitorAddr is the machine owner's local page. Loopback only, and
// enforced as such: it has no login because it is for whoever is sitting at
// the machine, so exposing it to the network would publish this computer's
// state and hand its pause control to the LAN.
const defaultMonitorAddr = "127.0.0.1:7821"

// stopGrace is how long a daemon gets to shut down cleanly. Long enough for
// jobs to be signalled and scratch to be cleaned, short enough to notice.
const stopGrace = 20 * time.Second

type upOpts struct {
	name       string
	port       int
	web        string
	ssh        string
	monitor    string
	advertise  string
	foreground bool
	controller bool // force controller role

	// set names the options the caller actually gave.
	//
	// Needed to tell "not mentioned" from "asked for the default", which is
	// what makes `shome restart` come back as the same cluster it was. It
	// used to start with the defaults for everything the command line did
	// not repeat, so a controller started on --port 7830 --no-web restarted
	// on 7817 with the console open -- and, where something else already
	// held 7817, did not come back at all.
	set map[string]bool
}

// given reports whether the caller named an option.
func (o upOpts) given(name string) bool { return o.set[name] }

const upUsage = `shome up - start shome on this machine

With no options it does the right thing: a machine that has joined a cluster
comes back up as that cluster's agent, and any other machine starts a
controller. It runs in the background; use 'shome down' to stop it.

  --name NAME       node name for this machine (default: its hostname)
  --port N          port for node agents (default: 7817; the one-command
                    join service uses the next port up)
  --web ADDR        admin console address (default: ` + defaultWebAddr + `)
  --no-web          do not serve the admin console
  --advertise NAME  extra name or IP for the TLS certificate, comma-separated;
                    needed only for addresses shome cannot discover itself,
                    such as a VPN address assigned later
  --controller      start a controller even if this machine has joined one
  --foreground      run in this terminal instead of the background

  SHOME_ROOT=PATH   where to keep state (default: a per-user shared directory)
`

func parseUpFlags(args []string) (upOpts, error) {
	o := upOpts{port: daemon.DefaultPort, web: defaultWebAddr, ssh: defaultSSHAddr,
		monitor: defaultMonitorAddr, set: map[string]bool{}}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-h" || a == "--help" {
			return o, errUsage(upUsage)
		}
		next := func() (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s needs a value", a)
			}
			i++
			return args[i], nil
		}
		// Accept --flag=value as well as --flag value; people type both.
		if k, v, ok := strings.Cut(a, "="); ok && strings.HasPrefix(a, "--") {
			args = append(args[:i], append([]string{k, v}, args[i+1:]...)...)
			a = k
		}
		var err error
		switch a {
		case "--name", "-n":
			o.name, err = next()
			o.set["name"] = true
		case "--port", "-p":
			var s string
			if s, err = next(); err == nil {
				if _, err = fmt.Sscanf(s, "%d", &o.port); err != nil {
					err = fmt.Errorf("invalid port %q", s)
				}
			}
			o.set["port"] = true
		case "--web":
			o.web, err = next()
			o.set["web"] = true
		case "--no-web":
			o.web = ""
			o.set["web"] = true
		case "--ssh":
			o.ssh, err = next()
			o.set["ssh"] = true
		case "--no-ssh":
			o.ssh = ""
			o.set["ssh"] = true
		case "--monitor":
			o.monitor, err = next()
			o.set["monitor"] = true
		case "--no-monitor":
			o.monitor = ""
			o.set["monitor"] = true
		case "--advertise":
			o.advertise, err = next()
			o.set["advertise"] = true
		case "--foreground", "-f":
			o.foreground = true
		case "--controller":
			o.controller = true
		default:
			return o, fmt.Errorf("unknown option %q for 'shome up'.\nRun 'shome up --help' for the list.", a)
		}
		if err != nil {
			return o, err
		}
	}
	return o, nil
}

// up starts whatever this machine should be running: a controller if it has
// never joined anyone else's cluster, or its agent if it has.
func up(args []string) error {
	o, err := parseUpFlags(args)
	if err != nil {
		return err
	}
	root := ctl.DefaultRoot()

	if pid, running := daemon.Status(root); running {
		fmt.Printf("shome is already running (pid %d).\n", pid)
		fmt.Printf("  state   %s\n  logs    shome logs -f\n", root)
		return nil
	}

	saved, hasSaved := daemon.LoadConfig(root)
	// A machine that joined someone else's cluster must come back up as that
	// cluster's agent. Starting a second controller there would silently
	// create a rival cluster of one, which is a confusing thing to debug.
	if hasSaved && saved.Role == daemon.RoleAgent && !o.controller {
		return upAgent(root, saved, o)
	}
	return upController(root, saved, o)
}

func upController(root string, saved daemon.SavedConfig, o upOpts) error {
	if o.name == "" {
		o.name = saved.Node
	}
	if o.name == "" {
		o.name = daemon.Hostname()
	}
	// How this machine was last started, for everything this command line
	// did not mention. A restart is meant to bring back the cluster that was
	// there, not a differently-configured one that happens to share its
	// state directory.
	listen := fmt.Sprintf("0.0.0.0:%d", o.port)
	if !o.given("port") && saved.Listen != "" {
		listen = saved.Listen
	}
	if !o.given("web") && saved.Role == daemon.RoleController {
		o.web = saved.WebAddr
	}
	if !o.given("ssh") && saved.Role == daemon.RoleController {
		o.ssh = saved.SSHAddr
	}
	if !o.given("monitor") && saved.Role == daemon.RoleController {
		o.monitor = saved.MonitorAddr
	}
	if !o.given("advertise") && saved.Advertise != "" {
		o.advertise = saved.Advertise
	}

	if !o.foreground {
		return relaunch(root, "controller", o.name, listen)
	}

	daemon.SaveConfig(root, daemon.SavedConfig{
		Role: daemon.RoleController, Node: o.name,
		Listen: listen, Advertise: o.advertise, WebAddr: o.web, SSHAddr: o.ssh,
		MonitorAddr: o.monitor,
	})
	ctx, log, done := foregroundSetup(root)
	defer done()
	return daemon.RunController(ctx, daemon.ControllerConfig{
		Root: root, Listen: listen, Node: o.name, LocalAgent: true,
		Advertise: o.advertise, WebAddr: o.web, SSHAddr: o.ssh,
		MonitorAddr: o.monitor, Bootstrap: true, Log: log,
	})
}

func upAgent(root string, saved daemon.SavedConfig, o upOpts) error {
	if !o.foreground {
		return relaunch(root, "agent", saved.Node, saved.Controller)
	}
	ctx, log, done := foregroundSetup(root)
	defer done()
	return daemon.RunAgent(ctx, daemon.AgentConfig{
		Root: root, Controller: saved.Controller, Node: saved.Node,
		ServerName: saved.ServerName, MonitorAddr: saved.MonitorAddr, Log: log,
	})
}

// foregroundSetup wires up signal handling, logging and the pid file for a
// daemon running in this process.
func foregroundSetup(root string) (context.Context, *slog.Logger, func()) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel()}))
	daemon.WritePID(root)
	return ctx, log, func() {
		daemon.ClearPID(root)
		stop()
	}
}

func logLevel() slog.Level {
	if os.Getenv("SHOME_DEBUG") != "" {
		return slog.LevelDebug
	}
	return slog.LevelInfo
}

// relaunch starts this same command again in the background and reports what
// happened, including the first sign of trouble from the log.
func relaunch(root, role, node, addr string) error {
	// `up`, whatever verb was typed. A child re-running `restart` would
	// stop and start again inside the daemon process, racing the pid file
	// this process is about to see -- and the flags mean the same thing to
	// both verbs, so there is nothing to translate.
	args := append([]string{"up"}, os.Args[2:]...)
	args = append(args, "--foreground")
	pid, exited, err := daemon.StartDetached(root, args)
	if err == daemon.ErrAlreadyRunning {
		fmt.Printf("shome is already running (pid %d)\n", pid)
		return nil
	}
	if err != nil {
		return err
	}

	// Confirm it is actually up rather than reporting success for a process
	// that died on startup -- a port clash or a bad address fails within
	// milliseconds, and "started" followed by silence is the worst outcome.
	if err := waitReady(root, exited, 10*time.Second); err != nil {
		return err
	}

	switch role {
	case "controller":
		fmt.Printf("shome controller running as node %q (pid %d)\n\n", node, pid)
		fmt.Printf("  listening   %s\n", addr)
		fmt.Printf("  state       %s\n", root)
		fmt.Printf("  logs        shome logs -f\n")
		if saved, ok := daemon.LoadConfig(root); ok {
			if saved.WebAddr != "" {
				fmt.Printf("  console     http://%s/\n", saved.WebAddr)
			}
			// What the login node actually did, asked of the daemon rather
			// than read from the configuration. Printing the configured
			// address regardless meant that when something else already held
			// the port -- an older shome that outlived its pid file, most
			// often -- this advertised an address answered by something
			// other than this cluster.
			if saved.SSHAddr != "" {
				li, err := loginState()
				switch {
				case err != nil:
					fmt.Printf("  login node  %s (could not confirm: %v)\n", saved.SSHAddr, err)
				case li.Enabled:
					fmt.Printf("  login node  ssh on %s\n", saved.SSHAddr)
				default:
					why := li.Reason
					if why == "" {
						why = "it did not report in time; see 'shome logs'"
					}
					fmt.Printf("  login node  NOT RUNNING: %s\n", why)
					fmt.Printf("              nobody can ssh in until that is resolved;\n")
					fmt.Printf("              'shome doctor' checks it, 'shome logs' has the detail\n")
				}
			}
			if saved.MonitorAddr != "" {
				fmt.Printf("  your view   http://%s/  (or: shome monitor)\n", saved.MonitorAddr)
			}
		}
		fmt.Printf("\nAdd another machine:  shome invite\n")
		fmt.Printf("Submit a job:         shome sbatch job.sh\n")
	default:
		fmt.Printf("shome agent running as node %q (pid %d)\n\n", node, pid)
		fmt.Printf("  controller  %s\n", addr)
		fmt.Printf("  state       %s\n", root)
		fmt.Printf("  logs        shome logs -f\n")
	}
	return nil
}

// loginState asks the daemon what became of its login node.
//
// Polled briefly, because the control socket starts answering before the
// login node has finished binding: an answer of neither "listening" nor a
// reason means it has not tried yet, and reporting that as "not running"
// was worse than waiting a moment for the truth.
func loginState() (ctl.LoginInfo, error) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		var li ctl.LoginInfo
		if err := call("GET", "/login", nil, &li); err != nil {
			return li, err
		}
		if li.Enabled || li.Reason != "" || time.Now().After(deadline) {
			return li, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// waitReady blocks until the daemon reports ready, or explains why it did not.
//
// Whatever the failure, the daemon's own last words are more useful than
// anything this function could say about it, so they are always included.
func waitReady(root string, exited <-chan error, timeout time.Duration) error {
	deadline := time.After(timeout)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		if _, running := daemon.Status(root); running {
			return nil
		}
		select {
		case <-exited:
			return fmt.Errorf("shome failed to start. Last lines of %s:\n\n%s",
				daemon.LogPath(root), indent(tailFile(daemon.LogPath(root), 15)))
		case <-deadline:
			return fmt.Errorf("shome did not report ready within %s. Last lines of %s:\n\n%s",
				timeout, daemon.LogPath(root), indent(tailFile(daemon.LogPath(root), 15)))
		case <-tick.C:
		}
	}
}

func indent(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		b.WriteString("    " + line + "\n")
	}
	return b.String()
}

// down stops this machine's daemon.
func down(args []string) error {
	root := ctl.DefaultRoot()
	pid, running := daemon.Status(root)
	if !running {
		// Fall back to the API, so a daemon started before pid files existed,
		// or one whose pid file was removed, can still be stopped.
		if err := call("POST", "/shutdown", nil, nil); err == nil {
			fmt.Println("shome stopped")
			return nil
		}
		fmt.Println("shome is not running on this machine")
		return nil
	}
	fmt.Printf("stopping shome (pid %d)... ", pid)
	stopped, err := stopWithTimeout(root)
	if err != nil {
		fmt.Println()
		return err
	}
	if stopped {
		fmt.Println("done")
	} else {
		fmt.Println("was not running")
	}
	return nil
}

func stopWithTimeout(root string) (bool, error) {
	return daemon.Stop(root, stopGrace)
}

// restart stops and starts, preserving the recorded role and settings.
func restart(args []string) error {
	root := ctl.DefaultRoot()
	if pid, running := daemon.Status(root); running {
		fmt.Printf("stopping shome (pid %d)... ", pid)
		if _, err := stopWithTimeout(root); err != nil {
			fmt.Println()
			return err
		}
		fmt.Println("done")
	}
	return up(args)
}

// logs shows the daemon's output.
func logs(args []string) error {
	root := ctl.DefaultRoot()
	path := daemon.LogPath(root)
	follow := false
	n := 40
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-f", "--follow":
			follow = true
		case "-n", "--lines":
			if i+1 >= len(args) {
				return fmt.Errorf("-n needs a number")
			}
			i++
			if _, err := fmt.Sscanf(args[i], "%d", &n); err != nil {
				return fmt.Errorf("invalid line count %q", args[i])
			}
		default:
			return fmt.Errorf("unknown option %q for 'shome logs'", args[i])
		}
	}
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("no log at %s.\nshome may never have been started here; try:  shome up", path)
	}
	if !follow {
		fmt.Print(tailFile(path, n))
		return nil
	}
	fmt.Print(tailFile(path, n))
	return followFile(path)
}

// tailFile returns the last n lines. Reads the whole file, which is fine for a
// log this size and avoids the complexity of a backwards scan.
func tailFile(path string, n int) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n") + "\n"
}

// followFile streams appended output until interrupted.
func followFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Seek(0, os.SEEK_END); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	buf := make([]byte, 8192)
	for ctx.Err() == nil {
		n, err := f.Read(buf)
		if n > 0 {
			os.Stdout.Write(buf[:n])
			continue
		}
		if err != nil && err.Error() != "EOF" {
			return err
		}
		select {
		case <-ctx.Done():
		case <-time.After(200 * time.Millisecond):
		}
	}
	return nil
}

var _ = filepath.Join

// errUsage carries help text that the top level prints on stdout without an
// error prefix. Asking for help is not an error, and should not read like one.
type errUsage string

func (e errUsage) Error() string { return string(e) }
