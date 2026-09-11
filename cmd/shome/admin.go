package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/davidwu/shome/internal/ctl"
	"github.com/davidwu/shome/internal/daemon"
	"github.com/davidwu/shome/internal/maccontainer"
	"github.com/davidwu/shome/internal/userenv"
)

func adminUsage() {
	fmt.Print(`shome admin - cluster administration

  token [-q]                  mint a single-use join token (-q: token only)
  hold JOBID                  stop a pending job being scheduled
  release JOBID               undo a hold
  requeue JOBID               kill and requeue a job
  drain NODE [reason]         stop a node accepting new work (running jobs continue)
  resume NODE                 let a node accept work again
  stop NODE                   ask a node's agent to exit cleanly
  forget NODE                 remove a departed node from the cluster
  node label NODE [NAME]      name a machine (no name shows it; --clear removes)
  node note NODE [TEXT]       a reminder to yourself, shown beside the machine

  audit [-n N] [--verify]     cluster-wide event log; --verify checks the hash chain
  quarantine NAME             revoke a user's token and cancel their jobs
  unquarantine NAME           restore a quarantined account
  emergency drain-all         stop the cluster taking new work
  sshd-config [DIR]           legacy: config for a separate system sshd
                              (shome up runs its own login node; you do not need this)
  user add NAME [--role admin|operator|user] [--max-LIMIT VALUE ...]
                                           create an account; see 'qos help'
                                           for the limits you can set
  user list                                list accounts
  key add NAME KEYFILE|"ssh-ed25519 ..."   let an account log in with that key
  key list [NAME]                          authorised SSH keys
  key rm FINGERPRINT                       revoke one key
  enroll NAME                              one-time code so they can register
                                           their own key on first ssh
  unenroll NAME FINGERPRINT                sign one of their machines out
  unenroll NAME --all                      sign out every machine AND cancel
                                           every unused code
  code rm ID                               cancel one unused enrollment code
  env [status|install|packages]            the environment logged-in accounts get
  priority [on|off|set|shares|explain]     how the queue is ordered (fair-share)
  qos [show|set|clear|file]                limits on what an account may use
  config [set NAME VALUE]                  how this cluster is reachable
  user del NAME                            delete an account (jobs are kept)
`)
}

func admin(args []string) error {
	if len(args) == 0 {
		adminUsage()
		return nil
	}
	switch args[0] {
	case "token":
		var out map[string]string
		if err := call("POST", "/token", nil, &out); err != nil {
			return err
		}
		// -q prints only the token, for scripting an unattended node join.
		for _, a := range args[1:] {
			if a == "-q" || a == "--quiet" {
				fmt.Println(out["token"])
				return nil
			}
		}
		fmt.Printf("join token (valid %s, single use):\n\n  %s\n\n", out["expires_in"], out["token"])
		fmt.Print("To add a machine, 'shome invite' prints a single command that\n" +
			"downloads shome and joins with this token. Otherwise, run there:\n" +
			"  shome join <this-host> --token " + out["token"] + "\n")
		return nil

	case "hold", "release", "requeue":
		if len(args) < 2 {
			return fmt.Errorf("%s needs a job id", args[0])
		}
		id, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil {
			return fmt.Errorf("invalid job id %q", args[1])
		}
		if err := call("POST", fmt.Sprintf("/job/%d/%s", id, args[0]), nil, nil); err != nil {
			return err
		}
		fmt.Printf("job %d %s\n", id, map[string]string{
			"hold": "held", "release": "released", "requeue": "requeued",
		}[args[0]])
		return nil

	case "drain":
		if len(args) < 2 {
			return fmt.Errorf("drain needs a node name")
		}
		q := "/node/" + args[1] + "/drain"
		if len(args) > 2 {
			q += "?reason=" + urlEscape(strings.Join(args[2:], " "))
		}
		if err := call("POST", q, nil, nil); err != nil {
			return err
		}
		fmt.Printf("node %s drained; running jobs continue\n", args[1])
		return nil

	case "stop":
		if len(args) < 2 {
			return fmt.Errorf("stop needs a node name")
		}
		if err := call("POST", "/node/"+args[1]+"/stop", nil, nil); err != nil {
			return err
		}
		fmt.Printf("asked %s to stop; running jobs continue until it exits\n", args[1])
		return nil

	case "forget":
		if len(args) < 2 {
			return fmt.Errorf("forget needs a node name")
		}
		if err := call("POST", "/node/"+args[1]+"/remove", nil, nil); err != nil {
			return err
		}
		fmt.Printf("removed %s from the cluster; its job history is kept\n", args[1])
		return nil

	case "node":
		return adminNode(args[1:])

	case "env":
		return adminEnv(args[1:])

	case "priority", "fairshare":
		return adminPriority(args[1:])

	case "resume":
		if len(args) < 2 {
			return fmt.Errorf("resume needs a node name")
		}
		if err := call("POST", "/node/"+args[1]+"/resume", nil, nil); err != nil {
			return err
		}
		fmt.Printf("node %s resumed\n", args[1])
		return nil

	case "sshd-config":
		root := ctl.DefaultRoot()
		if len(args) > 1 {
			root = args[1]
		}
		ssh := filepath.Join(root, "ssh")
		fmt.Print(sshdConfig(root,
			filepath.Join(ssh, "user_ca.pub"),
			filepath.Join(ssh, "login_host_key"),
			filepath.Join(ssh, "login_host_key-cert.pub")))
		return nil

	case "quarantine":
		return adminQuarantine(args[1:])
	case "unquarantine":
		return adminUnquarantine(args[1:])
	case "emergency":
		return adminEmergency(args[1:])
	case "audit":
		return adminAudit(args[1:])

	case "user":
		return adminUser(args[1:])
	case "key":
		return adminKey(args[1:])
	case "enroll", "invite-user":
		return adminEnroll(args[1:])
	case "unenroll":
		return adminUnenroll(args[1:])
	case "code":
		return adminCode(args[1:])
	case "qos", "limits":
		return adminQoS(args[1:])
	case "config":
		return adminConfig(args[1:])

	default:
		adminUsage()
		return fmt.Errorf("unknown admin command %q", args[0])
	}
}

func urlEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == ' ':
			b.WriteString("%20")
		case r == '&' || r == '?' || r == '#' || r == '=' || r == '+' || r == '%':
			fmt.Fprintf(&b, "%%%02X", r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func adminUser(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: shome admin user add|list|del ...")
	}
	switch args[0] {
	case "add":
		if len(args) < 2 {
			return fmt.Errorf("usage: shome admin user add NAME [--admin] [--quota SIZE]")
		}
		req := map[string]string{"name": args[1], "role": "user"}
		// Anything that is not a role flag is a QoS limit, applied to the new
		// account after it exists. `--quota` used to mean disk and nothing
		// else, which was never clear from the name; it is now --max-disk,
		// one of a set that all work the same way.
		var limitArgs []string
		for i := 2; i < len(args); i++ {
			switch {
			case args[i] == "--admin":
				req["role"] = "admin"
			case args[i] == "--role" && i+1 < len(args):
				i++
				req["role"] = args[i]
			case strings.HasPrefix(args[i], "--role="):
				req["role"] = strings.TrimPrefix(args[i], "--role=")
			case args[i] == "--quota" || strings.HasPrefix(args[i], "--quota="):
				v := strings.TrimPrefix(args[i], "--quota=")
				if v == "--quota" && i+1 < len(args) {
					i++
					v = args[i]
				}
				return fmt.Errorf("--quota has been replaced by --max-disk, which is one of a\n"+
					"full set of per-account limits:\n\n"+
					"  shome admin user add %s --max-disk %s\n\n"+
					"See them all with: shome admin qos help", args[1], v)
			default:
				limitArgs = append(limitArgs, args[i])
			}
		}
		limits, err := parseLimitFlags(limitArgs)
		if err != nil {
			return err
		}
		var out map[string]string
		if err := call("POST", "/users", req, &out); err != nil {
			return err
		}
		if len(limitArgs) > 0 {
			if err := call("POST", "/users/"+urlEscape(args[1])+"/qos", limits, nil); err != nil {
				return fmt.Errorf("account created, but its limits could not be set: %w", err)
			}
		}
		fmt.Printf("created %s (%s)\n\n", out["name"], req["role"])

		// Onboarding in one step. The token alone is not enough to be useful
		// from another computer -- the client API is a unix socket on this
		// machine -- so what a new person actually needs is the ssh command
		// and a code. Printing only the token used to send them looking for a
		// way to use it that does not exist.
		var e map[string]string
		if err := call("POST", "/users/"+args[1]+"/enroll", nil, &e); err == nil {
			fmt.Printf("Send them these two lines:\n\n")
			fmt.Printf("    ssh -p %d %s@%s\n", loginPort(), out["name"], loginHost())
			fmt.Printf("    enrollment code: %s\n\n", e["code"])
			fmt.Printf("They are asked for the code once; the key their ssh client already\n")
			fmt.Printf("offers is registered, and later logins need no code.\n")
			fmt.Printf("Single use, valid %s -- 'shome admin enroll %s' issues another.\n\n",
				e["expires_in"], out["name"])
		}
		fmt.Printf("API token, shown once (works on this machine, or via the SDK):\n\n  %s\n",
			out["token"])
		return nil

	case "list":
		var us []struct {
			Name     string `json:"name"`
			Role     string `json:"role"`
			QuotaMiB int64  `json:"quota_mib"`
			Disabled bool   `json:"disabled"`
		}
		if err := call("GET", "/users", nil, &us); err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tROLE\tQUOTA\tSTATUS")
		for _, u := range us {
			q := "unlimited"
			if u.QuotaMiB > 0 {
				q = fmt.Sprintf("%d MiB", u.QuotaMiB)
			}
			st := "active"
			if u.Disabled {
				st = "disabled"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", u.Name, u.Role, q, st)
		}
		return w.Flush()

	case "del":
		if len(args) < 2 {
			return fmt.Errorf("usage: shome admin user del NAME")
		}
		if err := call("DELETE", "/users/"+args[1], nil, nil); err != nil {
			return err
		}
		fmt.Printf("deleted %s (their job history is kept)\n", args[1])
		return nil
	}
	return fmt.Errorf("unknown: shome admin user %s", args[0])
}

// adminQuarantine freezes an account and stops its work in one action.
func adminQuarantine(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: shome admin quarantine NAME [--reason TEXT]")
	}
	name := args[0]
	q := "/users/" + name + "/quarantine"
	for i := 1; i < len(args); i++ {
		if args[i] == "--reason" && i+1 < len(args) {
			q += "?reason=" + urlEscape(args[i+1])
		}
	}
	var out struct {
		User      string  `json:"user"`
		Cancelled []int64 `json:"cancelled_jobs"`
	}
	if err := call("POST", q, nil, &out); err != nil {
		return err
	}
	fmt.Printf("quarantined %s: token revoked, %d running job(s) cancelled\n",
		out.User, len(out.Cancelled))
	fmt.Println("  their job history is kept; undo with: shome admin unquarantine " + name)
	return nil
}

func adminUnquarantine(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: shome admin unquarantine NAME")
	}
	if err := call("POST", "/users/"+args[0]+"/unquarantine", nil, nil); err != nil {
		return err
	}
	fmt.Printf("restored %s\n", args[0])
	return nil
}

// adminEmergency is the break-glass lever.
func adminEmergency(args []string) error {
	if len(args) == 0 || args[0] != "drain-all" {
		return fmt.Errorf("usage: shome admin emergency drain-all [--reason TEXT]")
	}
	q := "/emergency/drain-all"
	for i := 1; i < len(args); i++ {
		if args[i] == "--reason" && i+1 < len(args) {
			q += "?reason=" + urlEscape(args[i+1])
		}
	}
	var out struct {
		Drained []string `json:"drained"`
		Reason  string   `json:"reason"`
	}
	if err := call("POST", q, nil, &out); err != nil {
		return err
	}
	fmt.Printf("drained %d node(s): %s\n", len(out.Drained), strings.Join(out.Drained, ", "))
	fmt.Println("  running jobs continue; no new work will start")
	fmt.Println("  bring nodes back with: shome admin resume NODE")
	return nil
}

// adminAudit prints the cluster-wide event log.
func adminAudit(args []string) error {
	for _, a := range args {
		if a == "--verify" {
			var v struct {
				Intact  bool  `json:"intact"`
				Checked int   `json:"checked"`
				BadSeq  int64 `json:"first_bad_seq"`
			}
			if err := call("GET", "/audit/verify", nil, &v); err != nil {
				return err
			}
			if v.Intact {
				fmt.Printf("audit log intact: %d entries verified\n", v.Checked)
				return nil
			}
			return fmt.Errorf("audit log TAMPERED: entry %d does not match the chain "+
				"(%d entries checked)", v.BadSeq, v.Checked)
		}
	}
	n := "100"
	for i := 0; i < len(args); i++ {
		if args[i] == "-n" && i+1 < len(args) {
			n = args[i+1]
		}
	}
	var evs []struct {
		At     string `json:"At"`
		JobID  int64  `json:"JobID"`
		Kind   string `json:"Kind"`
		Detail string `json:"Detail"`
	}
	if err := call("GET", "/audit?n="+n, nil, &evs); err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "TIME\tJOB\tEVENT\tDETAIL")
	for i := len(evs) - 1; i >= 0; i-- {
		e := evs[i]
		ts := e.At
		if len(ts) > 19 {
			ts = ts[:19]
		}
		id := ""
		if e.JobID != 0 {
			id = fmt.Sprintf("%d", e.JobID)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", ts, id, e.Kind, e.Detail)
	}
	return w.Flush()
}

// adminKey manages which SSH public keys may log in to the login node.
//
// This is how a person who is not sitting at the controller gets access:
// the admin creates an account, authorises that person's public key against
// it, and they can ssh in as that account. There is no OS user anywhere.
func adminKey(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: shome admin key add|list|rm ...")
	}
	switch args[0] {
	case "add":
		if len(args) < 3 {
			return fmt.Errorf("usage: shome admin key add NAME KEYFILE\n" +
				"   or: shome admin key add NAME \"ssh-ed25519 AAAA... them@laptop\"\n\n" +
				"Ask them for the contents of ~/.ssh/id_ed25519.pub on their machine.")
		}
		name := args[1]
		key := strings.Join(args[2:], " ")
		// A path is far more convenient than pasting, so accept either and
		// work out which this is rather than making the caller say.
		if b, err := os.ReadFile(key); err == nil {
			key = strings.TrimSpace(string(b))
		}
		var out map[string]string
		if err := call("POST", "/keys", map[string]string{
			"user": name, "public_key": key,
		}, &out); err != nil {
			return err
		}
		fmt.Printf("authorised %s for %s\n", out["fingerprint"], out["user"])
		if c := out["comment"]; c != "" {
			fmt.Printf("  key comment: %s\n", c)
		}
		fmt.Printf("\nThey can now reach the cluster with:\n")
		fmt.Printf("  ssh -p %d %s@%s\n", loginPort(), out["user"], loginHost())
		return nil

	case "list":
		q := "/keys"
		if len(args) > 1 {
			q += "?user=" + urlEscape(args[1])
		}
		var res struct {
			Keys []struct {
				User        string `json:"user"`
				Fingerprint string `json:"fingerprint"`
				Comment     string `json:"comment"`
				LastUsed    string `json:"last_used"`
			} `json:"keys"`
			Codes []struct {
				User    string `json:"user"`
				ID      string `json:"id"`
				Expires string `json:"expires"`
			} `json:"codes"`
		}
		if err := call("GET", q, nil, &res); err != nil {
			return err
		}
		if len(res.Keys) == 0 && len(res.Codes) == 0 {
			fmt.Println("nobody can log in. give someone a code with: shome admin enroll NAME")
			return nil
		}
		if len(res.Keys) > 0 {
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "USER\tMACHINE\tFINGERPRINT\tLAST USED")
			for _, k := range res.Keys {
				used := k.LastUsed
				if used == "" {
					used = "never"
				} else if len(used) > 19 {
					used = strings.Replace(used[:19], "T", " ", 1)
				}
				name := k.Comment
				if name == "" {
					name = "-"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", k.User, name, k.Fingerprint, used)
			}
			w.Flush()
		}
		// Outstanding codes are shown alongside, because they are the other
		// way in: a listing of only registered machines answers half the
		// question somebody auditing access is asking.
		if len(res.Codes) > 0 {
			fmt.Println()
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "USER\tUNUSED CODE\tEXPIRES")
			for _, c := range res.Codes {
				exp := c.Expires
				if len(exp) > 19 {
					exp = strings.Replace(exp[:19], "T", " ", 1)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\n", c.User, c.ID, exp)
			}
			w.Flush()
			fmt.Println("\ncancel one with: shome admin code rm ID")
		}
		return nil

	case "rm", "remove", "revoke":
		if len(args) < 2 {
			return fmt.Errorf("usage: shome admin key rm FINGERPRINT   (see: shome admin key list)")
		}
		if err := call("DELETE", "/keys/"+args[1], nil, nil); err != nil {
			return err
		}
		fmt.Printf("revoked %s\n", args[1])
		return nil
	}
	return fmt.Errorf("unknown: shome admin key %s", args[0])
}

// loginHost and loginPort describe where to reach the login node, for the
// instructions printed after authorising a key.
func loginHost() string {
	if saved, ok := daemon.LoadConfig(ctl.DefaultRoot()); ok {
		for _, a := range strings.Split(saved.Advertise, ",") {
			if a = strings.TrimSpace(a); a != "" {
				return a
			}
		}
	}
	if ip := daemon.LANAddress(); ip != "" {
		return ip
	}
	return "<controller>"
}

func loginPort() int {
	if saved, ok := daemon.LoadConfig(ctl.DefaultRoot()); ok && saved.SSHAddr != "" {
		if _, p, err := net.SplitHostPort(saved.SSHAddr); err == nil {
			if n, err := strconv.Atoi(p); err == nil {
				return n
			}
		}
	}
	return 2222
}

// adminEnroll mints a one-time code so somebody can register their own SSH key
// the first time they connect.
//
// The alternative -- asking for their public key and adding it yourself --
// needs a second channel and a manual step before anything works. A code they
// redeem on first connect removes both, and grants nothing beyond registering
// one key for one account.
func adminEnroll(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: shome admin enroll NAME")
	}
	name := args[0]
	var out map[string]string
	if err := call("POST", "/users/"+name+"/enroll", nil, &out); err != nil {
		return err
	}
	fmt.Printf("Send %s these two lines:\n\n", name)
	fmt.Printf("    ssh -p %d %s@%s\n", loginPort(), name, loginHost())
	fmt.Printf("    enrollment code: %s\n\n", out["code"])
	fmt.Printf("On their first connection they are asked for the code, and the key\n")
	fmt.Printf("their client already offered is registered. After that they log in\n")
	fmt.Printf("normally with no code.\n\n")
	fmt.Printf("Single use, valid %s.\n", out["expires_in"])
	return nil
}

// adminUnenroll signs a machine out on somebody's behalf.
//
// For the cases the person cannot handle themselves: a lost or stolen laptop,
// a colleague who has left, a shared computer nobody should still be reaching
// the cluster from. Same operation as the user's own `unenroll`, including
// spending their outstanding enrollment codes -- revoking a key while leaving a
// live code behind would not be revoking anything.
func adminUnenroll(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: shome admin unenroll NAME FINGERPRINT\n" +
			"   or: shome admin unenroll NAME --all\n\n" +
			"See their machines with: shome admin key list NAME")
	}
	name := args[0]
	req := map[string]any{}
	target := ""
	for _, a := range args[1:] {
		switch {
		case a == "--all":
			req["all"] = true
			target = "every machine"
		case strings.HasPrefix(a, "-"):
			return fmt.Errorf("unknown option %q", a)
		default:
			req["fingerprint"] = a
			target = a
		}
	}
	if target == "" {
		return fmt.Errorf("say which machine: a fingerprint from 'shome admin key list %s', or --all", name)
	}

	var out struct {
		User    string   `json:"user"`
		Removed []string `json:"removed"`
		Codes   int      `json:"codes_invalidated"`
	}
	if err := call("POST", "/users/"+name+"/unenroll", req, &out); err != nil {
		return err
	}
	switch len(out.Removed) {
	case 0:
		fmt.Printf("%s had no machines enrolled\n", name)
	case 1:
		fmt.Printf("signed %s out of %s\n", name, out.Removed[0])
	default:
		fmt.Printf("signed %s out of %d machine(s):\n", name, len(out.Removed))
		for _, f := range out.Removed {
			fmt.Printf("  %s\n", f)
		}
	}
	if out.Codes > 0 {
		fmt.Printf("%d unused enrollment code(s) invalidated.\n", out.Codes)
	}
	fmt.Printf("\nTheir account, jobs and files are untouched.\n")
	fmt.Printf("To let them back in:  shome admin enroll %s\n", name)
	return nil
}

// adminCode cancels an outstanding enrollment code.
//
// Separate from unenroll because they undo different things: unenroll takes
// away a machine that already has access, this takes away the ability to grant
// one. Cancelling a code someone has not used yet -- because it went to the
// wrong person, or was sent over a channel you would rather it had not been --
// should not disturb the machines they are already working from.
func adminCode(args []string) error {
	if len(args) < 2 || (args[0] != "rm" && args[0] != "remove" && args[0] != "cancel") {
		return fmt.Errorf("usage: shome admin code rm ID   (see: shome admin key list)")
	}
	var out map[string]string
	if err := call("DELETE", "/codes/"+urlEscape(args[1]), nil, &out); err != nil {
		return err
	}
	fmt.Printf("cancelled code %s for %s\n", out["code"], out["user"])
	fmt.Printf("Their enrolled machines are unaffected.\n")
	return nil
}

// adminNode names machines.
//
// A label is a display name, not a rename: a machine's identity is the common
// name in its certificate, and changing that would invalidate its credentials
// and orphan its job history. The label is shown wherever a person reads a
// machine name and accepted wherever they type one, which is what "naming a
// machine" needs to mean to be useful.
func adminNode(args []string) error {
	if len(args) == 0 {
		return adminNodeShow()
	}
	switch args[0] {
	case "label", "note":
		field := args[0]
		if len(args) < 2 {
			return fmt.Errorf("usage: shome admin node %s NODE [%s]\n\n"+
				"  shome admin node label a-long-hostname mini\n"+
				"  shome admin node label mini --clear", field, strings.ToUpper(field))
		}
		node := args[1]
		rest := args[2:]
		clear := false
		var vals []string
		for _, a := range rest {
			if a == "--clear" || a == "-c" {
				clear = true
				continue
			}
			vals = append(vals, a)
		}
		if !clear && len(vals) == 0 {
			// No value: show what it is now, so `node label mini` reads.
			var out map[string]any
			if err := call("GET", "/cluster", nil, &out); err != nil {
				return err
			}
			return printNodeNames(out, node)
		}
		body := map[string]any{"node": node, "clear": clear}
		// Joined rather than args[2] alone: a note is a sentence, and
		// requiring quotes around it is a papercut nobody needs.
		body[field] = strings.Join(vals, " ")
		var out map[string]any
		if err := call("POST", "/cluster/node", body, &out); err != nil {
			return err
		}
		if clear {
			fmt.Printf("cleared the %s for %s\n", field, out["node"])
		} else {
			fmt.Printf("%s is now called %q\n", out["node"], out["label"])
			if field == "note" {
				fmt.Printf("note: %s\n", out["note"])
			}
		}
		if field == "label" && !clear {
			fmt.Printf("\nThat name now works anywhere a machine name does:\n")
			fmt.Printf("  shome fs ls %s:\n", body["label"])
			fmt.Printf("  shome admin drain %s\n", body["label"])
		}
		return nil
	case "show", "list":
		return adminNodeShow()
	}
	return fmt.Errorf("unknown: shome admin node %s\n\n"+
		"  node label NODE [NAME]   name a machine\n"+
		"  node note NODE [TEXT]    leave yourself a reminder\n"+
		"  node show                what every machine is called", args[0])
}

func adminNodeShow() error {
	var out map[string]any
	if err := call("GET", "/cluster", nil, &out); err != nil {
		return err
	}
	return printNodeNames(out, "")
}

func printNodeNames(out map[string]any, only string) error {
	fmt.Printf("cluster: %v\n\n", out["name"])
	nodes, _ := out["nodes"].([]any)
	if len(nodes) == 0 {
		fmt.Println("no machines in this cluster yet")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tIDENTITY\tNOTE")
	shown := 0
	for _, n := range nodes {
		m, _ := n.(map[string]any)
		id, _ := m["identity"].(string)
		label, _ := m["label"].(string)
		if only != "" && !strings.EqualFold(only, id) && !strings.EqualFold(only, label) {
			continue
		}
		shown++
		identity := id
		if label == id {
			// Nothing gained by printing the same string twice.
			identity = "-"
		}
		note, _ := m["note"].(string)
		fmt.Fprintf(w, "%s\t%s\t%s\n", label, identity, note)
	}
	w.Flush()
	if only != "" && shown == 0 {
		return fmt.Errorf("no machine called %q", only)
	}
	fmt.Printf("\nNAME is what you type and what listings show. IDENTITY is the\n")
	fmt.Printf("certificate name underneath, shown only where they differ.\n")
	return nil
}

// adminEnv manages the environment a logged-in account gets.
//
// Local rather than an API call: it copies binaries into this machine's
// managed directory, so it must run on the machine whose sessions it affects.
func adminEnv(args []string) error {
	root := ctl.DefaultRoot()
	verb := "status"
	if len(args) > 0 {
		verb = args[0]
	}
	switch verb {
	case "status", "show":
		fmt.Printf("session environment on this machine\n\n")
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "TOOL\tSTATE\tVERSION")
		for _, t := range userenv.Status(root) {
			state := "missing"
			if t.Present {
				state = "installed"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\n", t.Name, state, t.Version)
		}
		shome := "missing"
		if fi, err := os.Stat(userenv.ShomePath(root)); err == nil && !fi.IsDir() {
			shome = "installed"
		}
		fmt.Fprintf(w, "shome\t%s\t(kept current on every start)\n", shome)
		w.Flush()
		fmt.Printf("\ndirectory: %s\n", userenv.Dir(root))
		if !userenv.Ready(root) {
			fmt.Printf("\nAccounts logging in have no package manager yet.\n")
			fmt.Printf("Give them one with:  shome admin env install\n")
		} else {
			fmt.Printf("\nAn account that logs in gets a sandboxed shell with uv,\n")
			fmt.Printf("writable only inside its own directory.\n")
		}
		printNativeEnv()
		return nil

	case "install", "update":
		fmt.Printf("session environment updated:\n")
		// Everything the machine needs to run cluster work: shome's own
		// tools, uv, and -- on a machine whose package manager can be
		// driven without a password -- the base software set. The same
		// call a node makes for itself when it starts, so running this by
		// hand and joining a machine produce the same environment.
		userenv.Provision(context.Background(), root, func(msg string, args ...any) {
			fmt.Printf("  %s%s\n", msg, kvNote(args))
		})
		for _, t := range userenv.Status(root) {
			if t.Present {
				fmt.Printf("  %s %s\n", t.Name, t.Version)
			}
		}
		if msg, err := buildSessionImage(root); err != nil {
			// The tools are installed either way; say what is missing rather
			// than failing the whole command.
			fmt.Printf("\nwarning: %v\n", err)
		} else if msg != "" {
			fmt.Printf("  %s\n", msg)
		}
		fmt.Printf("\nLogged-in accounts get this on their next session.\n")
		return nil

	case "packages":
		return adminEnvPackages(root, args[1:])
	}
	return fmt.Errorf("unknown: shome admin env %s\n\n"+
		"  env status              what the environment offers\n"+
		"  env install             build it, and copy this machine's uv in\n"+
		"  env packages            the software a session gets\n"+
		"  env packages add PKG    add software for this cluster\n"+
		"  env packages rm PKG     remove something added earlier", verb)
}

// kvNote renders a log line's key/value pairs for a person reading a
// terminal rather than a log aggregator.
func kvNote(args []any) string {
	if len(args) == 0 {
		return ""
	}
	var parts []string
	for i := 0; i+1 < len(args); i += 2 {
		parts = append(parts, fmt.Sprintf("%v: %v", args[i], args[i+1]))
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, "; ") + ")"
}

// buildSessionImage builds the image sessions run in, if this machine uses
// one. Reports what it did, or nothing when there is nothing to do.
func buildSessionImage(root string) (string, error) {
	rt, why := maccontainer.Detect(context.Background(), "", "")
	if rt == nil {
		// Not a machine that runs sessions in containers -- a Linux node
		// uses the host's own software -- so there is no image to build.
		if why != "" && runtime.GOOS == "darwin" {
			return "", fmt.Errorf("no session image: %s", why)
		}
		return "", nil
	}
	extra, err := userenv.ExtraPackages(root)
	if err != nil {
		return "", err
	}
	pkgs, err := userenv.Packages(extra)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	tag, built, err := rt.BuildImage(ctx, userenv.ImageDir(root), maccontainer.DefaultImage, pkgs)
	if err != nil {
		return "", err
	}
	// Superseded images are ~320 MB each and would otherwise accumulate one
	// per change to the package set.
	pruned := rt.PruneImages(ctx, tag)
	note := ""
	if len(pruned) > 0 {
		note = fmt.Sprintf(", removed %d superseded", len(pruned))
	}
	if !built {
		return fmt.Sprintf("session image %s (already built, %d packages%s)",
			tag, len(pkgs), note), nil
	}
	return fmt.Sprintf("session image %s built with %d packages%s",
		tag, len(pkgs), note), nil
}

// adminEnvPackages shows and edits the software a session gets.
func adminEnvPackages(root string, args []string) error {
	if len(args) == 0 {
		extra, err := userenv.ExtraPackages(root)
		if err != nil {
			return err
		}
		all, err := userenv.Packages(extra)
		if err != nil {
			return err
		}
		fmt.Printf("software in every session (%d packages)\n\n", len(all))
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "PACKAGE\tSOURCE")
		base := map[string]bool{}
		for _, p := range userenv.BasePackages() {
			base[p] = true
		}
		for _, p := range all {
			src := "added here"
			if base[p] {
				src = "shome"
			}
			fmt.Fprintf(w, "%s\t%s\n", p, src)
		}
		w.Flush()
		fmt.Printf("\nPlus uv, and the shome commands. Accounts install their own\n")
		fmt.Printf("language-level tools with 'uv tool install', which persist in\n")
		fmt.Printf("their home directory; anything apt-installed inside a session\n")
		fmt.Printf("does not, because the container is discarded at logout.\n")
		fmt.Printf("\nfile: %s\n", userenv.PackagesFile(root))
		return nil
	}
	switch args[0] {
	case "add":
		if len(args) < 2 {
			return fmt.Errorf("usage: shome admin env packages add PKG [PKG...]")
		}
		// Validated before writing, so a typo is refused here rather than
		// failing the image build several minutes later.
		if _, err := userenv.Packages(args[1:]); err != nil {
			return err
		}
		added, err := userenv.AddExtraPackages(root, args[1:])
		if err != nil {
			return err
		}
		if len(added) == 0 {
			fmt.Println("already in the list; nothing to do")
			return nil
		}
		fmt.Printf("added: %s\n", strings.Join(added, " "))
		fmt.Printf("\nRun 'shome admin env install' to rebuild the image.\n")
		return nil
	case "rm", "remove":
		if len(args) < 2 {
			return fmt.Errorf("usage: shome admin env packages rm PKG [PKG...]")
		}
		removed, err := userenv.RemoveExtraPackages(root, args[1:])
		if err != nil {
			return err
		}
		fmt.Printf("removed: %s\n", strings.Join(removed, " "))
		fmt.Printf("\nRun 'shome admin env install' to rebuild the image.\n")
		return nil
	}
	return fmt.Errorf("unknown: shome admin env packages %s (add, rm)", args[0])
}

// printNativeEnv reports the base software a natively-run job finds.
//
// Worth its own section because it is the one part of the environment shome
// does not install. A GPU job on a Mac cannot run in a container -- Metal
// does not exist inside a virtual machine -- so it uses the machine's own
// copies, and what those are depends on the machine. An admin should be able
// to see the gap here rather than have a job find it.
func printNativeEnv() {
	tools := userenv.NativeStatus()
	if len(tools) == 0 {
		return
	}
	trees := userenv.MachineSoftware()
	var missing []string
	present := 0
	for _, t := range tools {
		if t.Path == "" {
			missing = append(missing, t.Command)
			continue
		}
		present++
	}
	// Which jobs this applies to depends on the machine. On a Mac it is
	// the GPU ones, which cannot be contained; on Linux there is no image
	// at all and it is every job -- saying "GPU jobs" there was telling a
	// a Linux machine's owner that a limitation applied to work their
	// machine cannot even take.
	if runtime.GOOS == "darwin" {
		fmt.Printf("\nGPU jobs run natively rather than in a container, because Metal\n")
		fmt.Printf("does not exist inside a virtual machine, so they use this machine's\n")
		fmt.Printf("own copies of the base software:\n\n")
	} else {
		fmt.Printf("\nJobs here run in a mount namespace over this machine's own /usr\n")
		fmt.Printf("rather than in an image, so they use its own copies of the base\n")
		fmt.Printf("software:\n\n")
	}
	fmt.Printf("  present:  %d of %d commands\n", present, len(tools))
	if len(missing) > 0 {
		fmt.Printf("  missing:  %s\n", strings.Join(missing, ", "))
	}
	if len(trees) > 0 {
		fmt.Printf("  found in: %s\n", strings.Join(userenv.SoftwareRoots(trees), ", "))
	}
	if len(missing) > 0 && runtime.GOOS == "darwin" {
		fmt.Printf("\nA GPU job will not have those. Installing them on this machine --\n")
		fmt.Printf("with Homebrew, or 'xcode-select --install' for the developer\n")
		fmt.Printf("tools -- is what makes them available; shome does not drive\n")
		fmt.Printf("Homebrew for you.\n")
	} else if len(missing) > 0 {
		fmt.Printf("\nJobs needing those will fail. 'shome admin env install' on this\n")
		fmt.Printf("machine installs them with its own package manager, which is also\n")
		fmt.Printf("what a node does for itself when it starts -- unless sudo here\n")
		fmt.Printf("needs a password, which a daemon cannot answer.\n")
	}
}
