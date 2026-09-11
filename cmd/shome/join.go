package main

import (
	"fmt"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/davidwu/shome/internal/ctl"
	"github.com/davidwu/shome/internal/daemon"
)

// invite and join are two halves of one operation: adding a machine.
//
// The old flow was six manual steps across two machines -- mint a token, find
// the controller's LAN address, cross-compile for the target, copy the binary,
// run an agent with four flags, then check it landed. Each step had a way to
// go subtly wrong, and two of them (which address, which certificate name)
// failed with errors that pointed at the wrong thing entirely. Now the
// controller prints one line and the other machine runs it.

const joinUsage = `shome join - connect this machine to a cluster

Usually you do not type this: 'shome invite' on the controller prints a single
command that installs shome here and runs this for you.

  shome join HOST[:PORT] --token TOKEN [--name NAME]

  --token TOKEN   the join token, from 'shome invite' or 'shome admin token'
  --name NAME     name for this machine (default: its hostname)
  --foreground    run in this terminal instead of the background

The port defaults to 7817. After joining once, 'shome up' reconnects with no
arguments.
`

// invite mints a join token and prints the single command to run elsewhere.
func invite(args []string) error {
	name := ""
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			return fmt.Errorf("usage: shome invite [node-name]")
		}
		name = a
	}

	var out map[string]string
	if err := call("POST", "/token", nil, &out); err != nil {
		return err
	}
	token := out["token"]
	if token == "" {
		return fmt.Errorf("the controller did not return a join token")
	}

	addr, err := inviteURL(token)
	if err != nil {
		return err
	}

	fmt.Printf("Run this on the machine you want to add:\n\n")
	if name != "" {
		fmt.Printf("    SHOME_NAME=%s curl -fsSL %s | sh\n\n", name, addr)
	} else {
		fmt.Printf("    curl -fsSL %s | sh\n\n", addr)
	}
	fmt.Printf("It downloads shome, joins this cluster, and starts running jobs.\n")
	fmt.Printf("Valid %s, and only once.\n\n", out["expires_in"])
	fmt.Print(localNetworkNote)
	fmt.Printf("Then check it arrived:  shome sinfo\n")
	return nil
}

// localNetworkNote warns about the macOS permission that blocks this.
//
// Printed on every invitation rather than only on failure, because the failure
// it causes is unrecognisable: recent macOS requires an app to be granted
// Local Network access, and without it a terminal can reach the internet
// perfectly while every connection to a machine on the same LAN fails
// instantly. It looks exactly like a firewall or a routing fault, and it is
// neither -- which cost a long afternoon to work out the first time.
const localNetworkNote = `If the machine you are adding is a Mac, grant its terminal Local Network
access first: System Settings > Privacy & Security > Local Network, and
enable Terminal or iTerm. Without it the download fails instantly even
though that Mac has working internet.

`

// inviteURL builds the bootstrap URL other machines should fetch.
//
// The port comes from what the controller was actually told to listen on
// rather than the default, because a cluster started on a non-standard port
// would otherwise hand out an invitation that cannot connect.
func inviteURL(token string) (string, error) {
	ip := daemon.LANAddress()
	if ip == "" {
		return "", fmt.Errorf("this machine has no network address other than loopback,\n" +
			"so no other machine can reach it. Connect to a network and try again.")
	}
	port := daemon.DefaultPort
	if saved, ok := daemon.LoadConfig(ctl.DefaultRoot()); ok && saved.Listen != "" {
		if _, p, err := net.SplitHostPort(saved.Listen); err == nil {
			if n, err := strconv.Atoi(p); err == nil {
				port = n
			}
		}
	}
	return fmt.Sprintf("http://%s%s%s/install",
		net.JoinHostPort(ip, strconv.Itoa(port+1)), ctl.BootstrapPathPrefix, token), nil
}

// join connects this machine to a controller and starts its agent.
func join(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: shome join CONTROLLER[:PORT] --token TOKEN [--name NAME]\n\n" +
			"Get the whole command by running 'shome invite' on the controller.")
	}
	controller := args[0]
	if controller == "-h" || controller == "--help" {
		return errUsage(joinUsage)
	}
	if strings.HasPrefix(controller, "-") {
		return fmt.Errorf("shome join needs the controller's address first, e.g.\n" +
			"  shome join 10.0.0.5 --token ...")
	}
	// A bare host means the standard port. Nobody should have to remember it.
	if _, _, err := net.SplitHostPort(controller); err != nil {
		controller = net.JoinHostPort(controller, strconv.Itoa(daemon.DefaultPort))
	}

	var token, name string
	foreground := false
	for i := 1; i < len(args); i++ {
		a := args[i]
		if k, v, ok := strings.Cut(a, "="); ok && strings.HasPrefix(a, "--") {
			args = append(args[:i], append([]string{k, v}, args[i+1:]...)...)
			a = k
		}
		next := func() (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s needs a value", a)
			}
			i++
			return args[i], nil
		}
		var err error
		switch a {
		case "--token", "-t":
			token, err = next()
		case "--name", "-n":
			name, err = next()
		case "--foreground", "-f":
			foreground = true
		case "-h", "--help":
			return errUsage(joinUsage)
		default:
			return fmt.Errorf("unknown option %q for 'shome join'.\nRun 'shome join --help' for the list.", a)
		}
		if err != nil {
			return err
		}
	}
	if name == "" {
		name = daemon.Hostname()
	}

	root := ctl.DefaultRoot()
	if pid, running := daemon.Status(root); running {
		return fmt.Errorf("shome is already running here (pid %d).\n"+
			"Stop it first if you want to join a different cluster:  shome down", pid)
	}

	// Joining twice is a normal thing to try; say so rather than failing on a
	// token that was already spent.
	if _, _, _, err := ctl.LoadAgentIdentity(ctl.AgentStatePath(root)); err == nil && token != "" {
		fmt.Println("this machine has already joined a cluster; reconnecting")
		token = ""
	}

	if !foreground {
		pid, exited, err := daemon.StartDetached(root, append(os.Args[1:], "--foreground"))
		if err == daemon.ErrAlreadyRunning {
			fmt.Printf("shome is already running (pid %d)\n", pid)
			return nil
		}
		if err != nil {
			return err
		}
		// Generous: joining does a TLS handshake, a certificate issue, and a
		// full hardware inventory before the agent reports ready.
		if err := waitReady(root, exited, 45*time.Second); err != nil {
			return fmt.Errorf("%w%s", err, joinHint(err))
		}
		fmt.Printf("joined %s as node %q (pid %d)\n\n", controller, name, pid)
		fmt.Printf("  state   %s\n", root)
		fmt.Printf("  logs    shome logs -f\n")
		fmt.Printf("  stop    shome down\n")
		fmt.Printf("\nWatch what the cluster does to this machine:\n")
		fmt.Printf("  shome monitor            interactive\n")
		fmt.Printf("  http://%s/   in a browser\n", defaultMonitorAddr)
		return nil
	}

	ctx, log, done := foregroundSetup(root)
	defer done()
	return daemon.RunAgent(ctx, daemon.AgentConfig{
		Root: root, Controller: controller, Token: token, Node: name,
		MonitorAddr: defaultMonitorAddr, Log: log,
	})
}

// joinHint adds the advice most likely to be relevant to a failed join.
//
// A connection failure here has one overwhelmingly common cause on macOS, and
// it is not one anybody guesses: Local Network permission. The symptom -- an
// instant failure to reach a machine on the same network, from a terminal
// whose internet works fine -- reads as a firewall or routing problem, and
// sends people to inspect their router for an afternoon.
func joinHint(err error) string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	msg := strings.ToLower(err.Error())
	// Connection-level failures only. "refused" alone is too broad: the
	// controller says "join refused" when a token has expired, and blaming
	// that on a permission sends the user to fix something already correct.
	for _, sign := range []string{
		"connection refused", "no route to host", "i/o timeout",
		"unreachable", "connect: ", "dial tcp", "couldn't connect",
	} {
		if strings.Contains(msg, sign) {
			return "\n\nOn macOS this is usually Local Network permission, not the network:\n" +
				"  System Settings > Privacy & Security > Local Network\n" +
				"  enable Terminal (or iTerm), then quit and reopen it.\n" +
				"Without it, connections to machines on your own LAN fail instantly\n" +
				"even though the internet works."
		}
	}
	return ""
}
