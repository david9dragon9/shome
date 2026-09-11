package main

import (
	"fmt"
	"os/exec"
	"runtime"

	"github.com/davidwu/shome/internal/ctl"
	"github.com/davidwu/shome/internal/daemon"
)

// `shome web` and `shome token` exist because the web console needs a
// credential, and the only way to get one used to be knowing where shome
// keeps its state and reading a file out of it. That is a path the user never
// chose and has no reason to remember -- and telling them to run
// `cat $SHOME_ROOT/admin.token` fails outright, because SHOME_ROOT is a
// default computed inside the program, not an environment variable anyone
// has set.

// webConsole prints how to reach the console, with the token to paste.
func webConsole(args []string) error {
	open := false
	for _, a := range args {
		switch a {
		case "--open", "-o":
			open = true
		default:
			return fmt.Errorf("unknown option %q for 'shome web'", a)
		}
	}

	root := ctl.DefaultRoot()
	saved, ok := daemon.LoadConfig(root)
	if !ok || saved.Role != daemon.RoleController {
		return fmt.Errorf("the web console runs on the controller, and this machine is not it.\n" +
			"Run 'shome web' there instead.")
	}
	if saved.WebAddr == "" {
		return fmt.Errorf("the console is disabled on this controller.\n" +
			"Enable it with:  shome restart --web 127.0.0.1:7820")
	}
	if _, running := daemon.Status(root); !running {
		fmt.Println("note: shome is not running here; start it with 'shome up'")
		fmt.Println()
	}

	url := "http://" + saved.WebAddr + "/"
	tok, err := validToken()
	if err != nil {
		return err
	}

	fmt.Printf("Web console  %s\n", url)
	fmt.Printf("Token        %s\n\n", tok)
	fmt.Printf("Open the URL and paste the token. It is your API credential:\n")
	fmt.Printf("treat it like a password, and do not paste it anywhere else.\n")
	if isLoopback(saved.WebAddr) {
		fmt.Printf("\nThe console listens on loopback only. To reach it from another\n")
		fmt.Printf("machine, tunnel rather than exposing it:\n")
		fmt.Printf("  ssh -N -L %s:%s you@%s\n",
			portOf(saved.WebAddr), saved.WebAddr, daemon.Hostname())
	}
	if open {
		if err := openBrowser(url); err != nil {
			fmt.Printf("\ncould not open a browser (%v); open the URL above yourself\n", err)
		}
	}
	return nil
}

// tokenCmd prints just the token, for scripting and for pasting.
func tokenCmd(args []string) error {
	for _, a := range args {
		return fmt.Errorf("unknown option %q for 'shome token'", a)
	}
	tok, err := validToken()
	if err != nil {
		return err
	}
	fmt.Println(tok)
	return nil
}

func isLoopback(addr string) bool {
	h := daemon.HostPart(addr)
	return h == "127.0.0.1" || h == "localhost" || h == "::1"
}

func portOf(addr string) string {
	h := daemon.HostPart(addr)
	if len(addr) > len(h)+1 {
		return addr[len(h)+1:]
	}
	return addr
}

// openBrowser is best-effort and deliberately not the default: a command that
// takes over the screen when someone only wanted to read a URL is a nuisance
// over SSH, which is where a controller often is.
func openBrowser(url string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", url).Start()
	case "linux":
		return exec.Command("xdg-open", url).Start()
	default:
		return fmt.Errorf("no browser opener for %s", runtime.GOOS)
	}
}
