package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/davidwu/shome/internal/ctl"
	"github.com/davidwu/shome/internal/daemon"
	"github.com/davidwu/shome/internal/owner"
)

// doctor diagnoses a shome installation in one command.
//
// Every heterogeneous node fails differently -- one Linux box lacks cgroup
// v2, a Mac lacks Full Disk Access, a laptop is on battery -- and chasing each in
// turn is the single biggest support cost this project would otherwise carry.
// Each check states what it looked at and, on failure, what to do about it.
// agentIdentityPresent reports whether this machine has joined a cluster as a
// worker, i.e. holds a node certificate issued by some controller.
func agentIdentityPresent(root string) bool {
	dir := ctl.AgentStatePath(root)
	if _, _, _, err := ctl.LoadAgentIdentity(dir); err == nil {
		return true
	}
	return false
}

func hasAnyToken() bool {
	t, err := validToken()
	return err == nil && t != ""
}

func doctor(args []string) error {
	var pass, warn, fail int
	check := func(name string, status rune, detail, fix string) {
		icon := map[rune]string{'p': "  ok  ", 'w': " warn ", 'f': " FAIL "}[status]
		fmt.Printf("[%s] %-34s %s\n", icon, name, detail)
		if fix != "" && status != 'p' {
			fmt.Printf("         -> %s\n", fix)
		}
		switch status {
		case 'p':
			pass++
		case 'w':
			warn++
		case 'f':
			fail++
		}
	}

	fmt.Printf("shome doctor -- %s/%s\n\n", runtime.GOOS, runtime.GOARCH)

	root := ctl.DefaultRoot()
	fmt.Printf("state directory: %s\n\n", root)

	// --- local installation ---
	if fi, err := os.Stat(root); err == nil && fi.IsDir() {
		check("state directory", 'p', "present", "")
	} else {
		check("state directory", 'w', "missing (not yet initialised)",
			"start it with 'shome up', or set SHOME_ROOT if it lives elsewhere")
	}

	sock := ctl.SocketPath(root)
	if len(sock) >= 104 {
		check("control socket path", 'f',
			fmt.Sprintf("%d bytes, over the OS limit of 104", len(sock)),
			"set SHOME_ROOT to a shorter path")
	} else {
		check("control socket path", 'p', fmt.Sprintf("%d bytes", len(sock)), "")
	}

	// --- what is this machine's role? ---
	//
	// A worker node has no local controller and needs no token: it talks to a
	// remote controller over mTLS with its own certificate. Reporting the
	// absence of a local controller as a FAILURE there is wrong, and it is
	// what a Linux node joining a Mac's cluster will see first.
	joined := agentIdentityPresent(root)
	localCtl := false
	if _, err := os.Stat(sock); err == nil {
		if c, derr := net.DialTimeout("unix", sock, 2*time.Second); derr == nil {
			c.Close()
			localCtl = true
		}
	}

	switch {
	case localCtl:
		check("role", 'p', "controller (and reachable)", "")
	case joined:
		check("role", 'p', "worker node, joined to a remote controller", "")
	default:
		check("role", 'w', "not part of a cluster yet",
			"to start a cluster here:   shome up\n"+
				"         to join an existing one: run 'shome invite' on the controller")
	}

	if localCtl {
		if _, err := os.Stat(sock); err != nil {
			check("controller", 'f', "socket vanished", "restart it with: shome restart")
		}
	} else if _, err := os.Stat(sock); err == nil {
		check("controller", 'w', "stale socket at "+sock,
			"left by an unclean shutdown; harmless, removed on next start")
	}

	// --- credentials ---
	// Only meaningful for a machine that talks to the API. A worker node
	// authenticates with its node certificate, not a user token.
	if !localCtl && !hasAnyToken() {
		if joined {
			check("API token", 'p', "not needed on a worker node", "")
		} else {
			check("API token", 'w', "none (only needed to run CLI commands here)",
				"set SHOME_TOKEN, or write it to ~/.shome/token")
		}
	} else if tok, err := validToken(); err != nil {
		check("API token", 'f', err.Error(),
			"set SHOME_TOKEN, or write it to ~/.shome/token")
	} else {
		var who map[string]any
		if err := call("GET", "/whoami", nil, &who); err != nil {
			check("API token", 'f', "rejected: "+err.Error(),
				"ask an admin for a new token: shome admin user add <you>")
		} else {
			check("API token", 'p', fmt.Sprintf("%v (%v)", who["user"], who["role"]), "")
		}
		_ = tok
	}

	// --- the login node ---
	// Only on a controller, which is where it runs, and only when one was
	// configured: a cluster deliberately without remote access is not
	// broken.
	//
	// Worth a check of its own because it fails quietly and completely: the
	// port is either free or held by something else, and when it is held --
	// most often by an older shome that outlived its pid file -- everything
	// else about the cluster works while nobody can ssh in, and the address
	// they are given answers with somebody else's login node.
	if localCtl {
		if saved, ok := daemon.LoadConfig(root); ok && saved.SSHAddr != "" {
			li, err := loginState()
			switch {
			case err != nil:
				check("login node", 'w', "could not ask the controller: "+err.Error(), "")
			case li.Enabled:
				check("login node", 'p', fmt.Sprintf("listening on port %d", li.Port), "")
			default:
				why := li.Reason
				if why == "" {
					why = "it did not report in time"
				}
				check("login node", 'f', "not running: "+why,
					"nobody can ssh in. If the address is in use, find what holds it:\n"+
						"         lsof -nP -iTCP:"+portOf(saved.SSHAddr)+" -sTCP:LISTEN")
			}
		}
	}

	// --- clock ---
	// Certificates and job timing both assume a sane clock; on home machines
	// that have been asleep this is a real failure mode.
	if out, err := exec.Command("date", "+%s").Output(); err == nil {
		_ = out
		check("system clock", 'p', time.Now().Format(time.RFC3339), "")
	}

	// --- platform specifics ---
	doctorPlatform(check)

	// --- owner policy ---
	m := owner.NewManager(root, owner.DarwinSensor{})
	d := m.Evaluate()
	policyPath := filepath.Join(root, owner.PolicyFile)
	switch {
	case strings.Contains(d.Reason, "invalid"):
		check("owner policy", 'f', d.Reason, "fix "+policyPath+"; the node is suspended until you do")
	case d.Action == owner.Run:
		check("owner policy", 'p', d.Reason, "")
	default:
		check("owner policy", 'w', fmt.Sprintf("%s - %s", d.Action, d.Reason),
			"this is your own policy; shome resume, or edit "+policyPath)
	}
	if dead := m.DeadTriggers(); len(dead) > 0 {
		check("owner yield rules", 'w',
			fmt.Sprintf("%d rule(s) cannot fire here: %s", len(dead), strings.Join(dead, ", ")),
			"shome does not read live power/input/pressure signals on "+runtime.GOOS+
				" yet; caps, schedules and `shome pause` still work")
	} else if !m.SensorLive() {
		check("owner yield rules", 'p', "none configured (live signals unavailable here)", "")
	}

	fmt.Printf("\n%d ok, %d warnings, %d failures\n", pass, warn, fail)
	if fail > 0 {
		return fmt.Errorf("%d check(s) failed", fail)
	}
	return nil
}
