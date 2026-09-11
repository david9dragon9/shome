package main

import (
	"fmt"
	"os"
	"strings"
)

// enrollCmd gives the caller a code to register another of their computers.
//
// People have more than one machine. Without this, adding a laptop means
// asking an admin for a code every time -- which makes a cluster feel
// administered rather than used, and puts a person in the loop for something
// that grants no new authority: whoever is already authenticated as this
// account could register a key anyway.
func enrollCmd(args []string) error {
	for _, a := range args {
		return fmt.Errorf("unknown option %q for 'shome enroll'", a)
	}
	var out map[string]string
	if err := call("POST", "/enroll", nil, &out); err != nil {
		return err
	}
	fmt.Printf("Run this on your other computer:\n\n")
	fmt.Printf("    ssh -p %d %s@%s\n", loginPort(), out["user"], loginHost())
	fmt.Printf("    enrollment code: %s\n\n", out["code"])
	fmt.Printf("It asks for the code once, registers that machine's key, and\n")
	fmt.Printf("afterwards logs in without one.\n\n")
	fmt.Printf("Single use, valid %s.\n", out["expires_in"])
	return nil
}

// keysCmd lists the keys that can log in as the caller, so somebody can see
// which of their machines have access and notice one they do not recognise.
func keysCmd(args []string) error {
	for _, a := range args {
		return fmt.Errorf("unknown option %q for 'shome keys'", a)
	}
	var res struct {
		Keys []struct {
			Fingerprint string `json:"fingerprint"`
			Comment     string `json:"comment"`
			LastUsed    string `json:"last_used"`
		} `json:"keys"`
		Codes []struct {
			ID      string `json:"id"`
			Expires string `json:"expires"`
		} `json:"codes"`
	}
	if err := call("GET", "/keys", nil, &res); err != nil {
		return err
	}
	if len(res.Keys) == 0 && len(res.Codes) == 0 {
		fmt.Println("no computers registered. add this one with: shome enroll")
		return nil
	}
	if len(res.Keys) > 0 {
		fmt.Println("computers that can log in as you:")
		for _, k := range res.Keys {
			used := k.LastUsed
			if used == "" {
				used = "never used"
			} else if len(used) > 19 {
				used = "last used " + strings.Replace(used[:19], "T", " ", 1)
			}
			name := k.Comment
			if name == "" {
				name = "-"
			}
			fmt.Printf("  %-22s %s  (%s)\n", name, k.Fingerprint, used)
		}
	}
	// Unused codes are shown too: each one is a computer that could still be
	// added, which is as much a part of "who can reach my account" as the
	// machines already registered.
	if len(res.Codes) > 0 {
		fmt.Println("\nunused enrollment codes (each can add one more computer):")
		for _, c := range res.Codes {
			exp := c.Expires
			if len(exp) > 19 {
				exp = strings.Replace(exp[:19], "T", " ", 1)
			}
			fmt.Printf("  %s  expires %s\n", c.ID, exp)
		}
	}
	fmt.Printf("\nSign this computer out:   shome unenroll\n")
	fmt.Printf("Sign out everywhere:      shome unenroll --all\n")
	return nil
}

// unenrollCmd signs a computer out of the cluster.
//
// The motivating case is a public or borrowed machine: leaving a key
// registered there hands the next person your cluster access. Signing out
// removes that key and, deliberately, spends every outstanding enrollment code
// for the account -- an unused code left alive would let whoever comes next
// simply enroll again.
//
// The account survives untouched. Jobs keep running, storage and quota are
// unchanged; only the ability of this machine to log in goes away.
func unenrollCmd(args []string) error {
	all := false
	for _, a := range args {
		switch a {
		case "--all":
			all = true
		default:
			return fmt.Errorf("unknown option %q for 'shome unenroll'", a)
		}
	}

	req := map[string]any{}
	if all {
		req["all"] = true
	} else {
		// The login node tells us which key opened this session. Without one
		// there is no "this computer" to sign out of -- which is the case on
		// the controller itself, where the CLI authenticates with a token.
		fp := strings.TrimSpace(os.Getenv("SHOME_SESSION_KEY"))
		if fp == "" {
			return fmt.Errorf("this session did not sign in with an SSH key, so there is\n" +
				"no single computer to sign out of.\n\n" +
				"  shome unenroll --all    remove every key for your account")
		}
		req["fingerprint"] = fp
	}

	var out struct {
		Removed []string `json:"removed"`
		Codes   int      `json:"codes_invalidated"`
	}
	if err := call("POST", "/unenroll", req, &out); err != nil {
		return err
	}
	switch len(out.Removed) {
	case 0:
		fmt.Println("nothing to sign out; no keys were registered")
	case 1:
		fmt.Printf("signed out: %s\n", out.Removed[0])
	default:
		fmt.Printf("signed out of %d computer(s):\n", len(out.Removed))
		for _, f := range out.Removed {
			fmt.Printf("  %s\n", f)
		}
	}
	if out.Codes > 0 {
		fmt.Printf("%d unused enrollment code(s) invalidated.\n", out.Codes)
	}
	fmt.Printf("\nYour account, jobs and files are untouched.\n")
	fmt.Printf("To get back in, ask an admin for a new code: shome admin enroll <you>\n")
	return nil
}
