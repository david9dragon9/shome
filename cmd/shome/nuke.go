package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/davidwu/shome/internal/agent"
	"github.com/davidwu/shome/internal/ctl"
	"github.com/davidwu/shome/internal/daemon"
)

// nuke removes every trace of shome from this machine, then proves it.
//
// The reversibility promise is a design constraint, not a courtesy: the
// account-based isolation design was abandoned precisely because it could not
// be undone without Recovery Mode. This verifies rather than asserts, because
// during development a teardown script once printed "removed" immediately
// after seven permission failures and only a residue check caught it.
//
// It also has to find things, not just delete a known path. shome installs a
// binary, may symlink six Slurm shims into a directory chosen at the time, and
// writes credentials under ~/.ssh and ~/.shome. Leaving any of those behind
// means "removed" is not true, and the user discovers it months later when a
// stale `sbatch` on their PATH fails strangely.

// agentStopWait is how long to give remote agents to notice a stop action
// before the controller they heard it from disappears. One heartbeat interval
// plus slack; missing it costs an orphaned process, not data.
const agentStopWait = 8 * time.Second

// item is one thing to remove.
type item struct {
	what  string // human description
	path  string
	bytes int64
}

// plan is everything shome has left on this machine.
type plan struct {
	root     string
	items    []item
	profiles []string // shell files mentioning shome; reported, never edited
	node     string   // this machine's cluster name, if it has one
	isAgent  bool
	remotes  []string // other machines in this cluster, when we are the controller
}

func nuke(args []string) error {
	confirmed, keepBinaries := false, false
	for _, a := range args {
		switch a {
		case "--confirm", "-y":
			confirmed = true
		case "--keep-binaries":
			keepBinaries = true
		default:
			return fmt.Errorf("unknown option %q for 'shome nuke'", a)
		}
	}

	p := survey(keepBinaries)

	if !confirmed {
		printPlan(p, keepBinaries)
		return nil
	}
	return execute(p)
}

// isolationStopWait bounds stopping a container, which is a virtual machine
// being torn down rather than a process being signalled.
const isolationStopWait = 30 * time.Second

// survey finds everything shome has put on this machine.
func survey(keepBinaries bool) plan {
	root := ctl.DefaultRoot()
	p := plan{root: root}
	if cfg, ok := daemon.LoadConfig(root); ok {
		p.node = cfg.Node
		p.isAgent = cfg.Role == daemon.RoleAgent
	}

	add := func(what, path string) {
		if path == "" {
			return
		}
		fi, err := os.Lstat(path)
		if err != nil {
			return
		}
		p.items = append(p.items, item{what: what, path: path, bytes: sizeOf(path, fi)})
	}

	add("state directory", root)

	home, _ := os.UserHomeDir()
	if home != "" {
		// ~/.shome is shared, not ours alone: a controller and an agent on the
		// same machine both keep a pointer there. Removing the directory
		// wholesale took the other role's pointer with it and left a running
		// controller that the CLI could no longer find. Take only the files
		// belonging to this installation, and the directory itself only if
		// nothing else is left in it.
		dot := filepath.Join(home, ".shome")
		add("API token", filepath.Join(dot, "token"))
		// The cross-built agents a controller hands to joining machines.
		// Skipped on a worker: an agent never serves them, so a worker
		// uninstalling itself on a machine that also runs a controller would
		// otherwise break every future `shome invite` with a 404.
		if !p.isAgent {
			add("agent binaries served to joining machines", filepath.Join(dot, "dist"))
		}
		for _, kind := range []string{"controller", "node"} {
			pt := filepath.Join(dot, kind)
			if b, err := os.ReadFile(pt); err == nil && strings.TrimSpace(string(b)) == root {
				add(kind+" pointer", pt)
			}
		}
		// SSH client credentials are the user's, but shome generated them and
		// they are useless without it.
		for _, n := range []string{"id_shome", "id_shome.pub", "id_shome-cert.pub"} {
			add("SSH credential", filepath.Join(home, ".ssh", n))
		}
	}

	// Resolve once: every "is this ours?" decision below compares against it.
	self := ""
	if exe, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			self = resolved
		} else {
			self = exe
		}
	}
	for _, l := range findShims(self) {
		add("Slurm shim", l)
	}
	if !keepBinaries {
		for _, b := range findBinaries(self) {
			add("binary", b)
		}
	}
	p.profiles = findProfileMentions(home)
	if !p.isAgent {
		p.remotes = otherNodes(p.node)
	}
	return p
}

// otherNodes lists the machines in this cluster besides this one.
//
// Removing a controller without telling its agents leaves them running
// forever, retrying a connection to something that no longer exists -- exactly
// the orphaned-process problem an uninstaller is supposed to prevent.
func otherNodes(self string) []string {
	var nodes []ctl.NodeInfo
	if err := call("GET", "/node", nil, &nodes); err != nil {
		return nil
	}
	var out []string
	for _, n := range nodes {
		if n.Name != self {
			out = append(out, n.Name)
		}
	}
	sort.Strings(out)
	return out
}

// findShims locates symlinks named after the Slurm commands that point at a
// shome binary. Scanning PATH rather than remembering where they were
// installed is the only way this works when the shims outlive the shell
// history that created them.
func findShims(self string) []string {
	if self == "" {
		return nil
	}
	self = resolve(self)
	var found []string
	seen := map[string]bool{}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			continue
		}
		for _, n := range shimNames {
			link := filepath.Join(dir, n)
			if seen[link] {
				continue
			}
			if _, err := os.Readlink(link); err != nil {
				continue // not a symlink, so not something we created
			}
			// Must resolve to this very binary. Matching on the link's name
			// instead would delete a real Slurm install's commands, or shims
			// belonging to a different shome the user meant to keep.
			if resolved, err := filepath.EvalSymlinks(link); err == nil && resolved == self {
				seen[link] = true
				found = append(found, link)
			}
		}
	}
	sort.Strings(found)
	return found
}

// findBinaries locates the shome executables belonging to THIS installation.
//
// Only the running binary and its sibling shome-shell -- never every file
// named "shome" found anywhere on PATH. Scanning PATH was the first attempt
// and it is too broad: a second installation the user deliberately keeps, or
// an unrelated program of the same name, would be removed by an uninstall
// that promised to touch nothing beyond its own state. An uninstaller should
// remove itself, not everything that resembles it.
//
// Unlinking a running binary is fine on Unix: the file goes, the process
// finishes.
func findBinaries(self string) []string {
	if self == "" {
		return nil
	}
	self = resolve(self)
	var found []string
	for _, p := range []string{self, filepath.Join(filepath.Dir(self), "shome-shell")} {
		if fi, err := os.Lstat(p); err == nil && !fi.IsDir() {
			found = append(found, p)
		}
	}
	sort.Strings(found)
	return found
}

// resolve follows symlinks, falling back to the path as given.
//
// Applied to both sides of every "is this ours?" comparison. Without it the
// check fails on macOS for a reason that has nothing to do with shome: /tmp
// and /var are symlinks into /private, so the same file has two absolute
// paths and a string comparison between them is simply wrong.
func resolve(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// findProfileMentions reports shell startup files that reference shome.
//
// Reported, never edited. Rewriting someone's shell configuration is not a
// thing an uninstaller should do unasked -- a bad edit there breaks every new
// terminal, and the line in question is one harmless PATH entry.
func findProfileMentions(home string) []string {
	if home == "" {
		return nil
	}
	var found []string
	for _, n := range []string{".profile", ".bash_profile", ".bashrc", ".zshrc", ".zprofile"} {
		path := filepath.Join(home, n)
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if strings.Contains(string(b), "shome") {
			found = append(found, path)
		}
	}
	return found
}

func sizeOf(path string, fi os.FileInfo) int64 {
	if !fi.IsDir() {
		return fi.Size()
	}
	var total int64
	filepath.WalkDir(path, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

func human(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.0f MiB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.0f KiB", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

func printPlan(p plan, keepBinaries bool) {
	if len(p.items) == 0 {
		fmt.Println("shome is not installed on this machine; nothing to remove.")
		return
	}
	fmt.Printf("This will remove shome from this machine:\n\n")
	var total int64
	for _, it := range p.items {
		fmt.Printf("  %-38s %8s  %s\n", it.what, human(it.bytes), it.path)
		total += it.bytes
	}
	fmt.Printf("\n  %-38s %8s\n", "total", human(total))

	fmt.Printf("\nAlso:\n")
	if pid, running := daemon.Status(p.root); running {
		fmt.Printf("  - shome is running (pid %d) and will be stopped first\n", pid)
	}
	if p.isAgent && p.node != "" {
		fmt.Printf("  - %s will be removed from the cluster's node list\n", p.node)
	}
	for _, n := range p.remotes {
		fmt.Printf("  - %s will be told to stop, so it is not left retrying forever\n", n)
	}
	fmt.Printf("  - job history, users, tokens, certificates and stored files go with it\n")

	fmt.Printf("\nIt does NOT touch:\n")
	fmt.Printf("  - your system SSH configuration\n")
	fmt.Printf("  - any OS account (shome never creates one on macOS)\n")
	fmt.Printf("  - anything outside the paths listed above\n")
	if keepBinaries {
		fmt.Printf("  - the shome binary itself (--keep-binaries)\n")
	}
	for _, f := range p.profiles {
		fmt.Printf("  - %s, which mentions shome; remove the PATH line yourself if you want\n", f)
	}
	fmt.Printf("\nRe-run with --confirm to proceed.\n")
}

// leaveCluster retires this node using the certificate it was issued at join.
func leaveCluster(p plan) error {
	cfg, ok := daemon.LoadConfig(p.root)
	if !ok || cfg.Controller == "" {
		return fmt.Errorf("this machine has no record of which controller it joined")
	}
	caPEM, certPEM, keyPEM, err := ctl.LoadAgentIdentity(ctl.AgentStatePath(p.root))
	if err != nil {
		return fmt.Errorf("no cluster identity on this machine: %w", err)
	}
	serverName := cfg.ServerName
	if serverName == "" {
		serverName = daemon.HostPart(cfg.Controller)
	}
	cli, err := agent.NewClient(cfg.Controller, p.node, caPEM, certPEM, keyPEM, serverName)
	if err != nil {
		return err
	}
	// Short: the point is to be tidy, not to block an uninstall on a machine
	// that may well be offline, which is often exactly why it is leaving.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return cli.Leave(ctx)
}

func execute(p plan) error {
	// Everything that needs a live control plane happens first. Ordering is
	// the whole trick here: stopping our own daemon before telling the other
	// machines to stop would leave them running with nothing to talk to.

	// Shut the other machines down while there is still something for them to
	// hear it from. Agents act on this at their next heartbeat.
	for _, n := range p.remotes {
		if err := call("POST", "/node/"+n+"/stop", nil, nil); err == nil {
			fmt.Printf("asked %s to stop\n", n)
		} else {
			fmt.Printf("note: could not reach %s to stop it; run 'shome down' there\n", n)
		}
	}
	if len(p.remotes) > 0 {
		fmt.Printf("waiting for them to pick it up... ")
		time.Sleep(agentStopWait)
		fmt.Println("done")
	}

	// Now the local daemon, so nothing rewrites state while it is removed.
	//
	// By pid, not by API call: a controller whose socket is already broken --
	// which is exactly when someone reaches for nuke -- would otherwise be
	// left running against a state directory that no longer exists, writing
	// into a deleted tree and holding the port against a fresh start.
	if pid, running := daemon.Status(p.root); running {
		fmt.Printf("stopping shome (pid %d)... ", pid)
		if _, err := daemon.Stop(p.root, stopGrace); err != nil {
			fmt.Println()
			return fmt.Errorf("%w\n\nNothing was removed. Stop it and run nuke again.", err)
		}
		fmt.Println("done")
	} else if err := call("POST", "/shutdown", nil, nil); err == nil {
		fmt.Println("asked the controller to stop")
	}

	// Only now say goodbye, and only after our own agent has stopped.
	//
	// Order matters: deregistering while the agent is still running means its
	// next heartbeat re-creates the node record, and the machine reappears in
	// sinfo seconds after being removed. Stop first, then leave.
	//
	// Over the agent's own mTLS channel, not the client API: the client API is
	// a unix socket to a controller on THIS machine, which a worker by
	// definition does not have. The first version of this used the client API
	// and failed with a confusing complaint about a missing API token, when
	// the real problem was that nothing was listening on the socket at all.
	//
	// Best effort: an unreachable controller must never be what stops someone
	// removing software from their own computer.
	if p.isAgent && p.node != "" {
		if err := leaveCluster(p); err == nil {
			fmt.Printf("told the controller %s is leaving\n", p.node)
		} else {
			fmt.Printf("note: could not reach the controller to say this node is leaving\n")
			fmt.Printf("      (%v)\n", err)
			fmt.Printf("      run 'shome admin forget %s' there when convenient\n", p.node)
		}
	}

	if p.root == "" || p.root == "/" || !strings.Contains(p.root, "shome") {
		return fmt.Errorf("refusing to remove %q: does not look like a shome root", p.root)
	}

	// Named as they go. The plan is printed when nuke is run without
	// --confirm, but somebody who typed --confirm straight off sees only a
	// count -- and one of the things in that count is the binary they just
	// ran, which is worth saying out loud rather than leaving them to
	// discover that `shome` is gone.
	fmt.Println("\nremoving")
	var failed []string
	for _, it := range p.items {
		if err := os.RemoveAll(it.path); err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", it.path, err))
			continue
		}
		fmt.Printf("  %-14s %s\n", it.what, it.path)
	}
	// Remove ~/.shome only when nothing else is using it. os.Remove on a
	// directory fails unless it is empty, which is exactly the test wanted.
	if home, err := os.UserHomeDir(); err == nil {
		os.Remove(filepath.Join(home, ".shome"))
	}

	// Isolation that outlives its launcher: on a Mac, a container is a
	// virtual machine, and one left running holds its memory until the
	// machine is rebooted. The daemon stops its own on the way down, so
	// this is for the case nuke exists to handle -- a daemon that was
	// already gone, or one that could not.
	//
	// After the state directory has gone, which is safe: the sweep
	// identifies containers by the name it gave them, and that name carries
	// this installation's tag rather than anything it has to read from disk.
	if be, err := newLocalBackend(p.root); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), isolationStopWait)
		defer cancel()
		if stopped := be.SweepOrphanIsolation(ctx, nil); len(stopped) > 0 {
			for _, n := range stopped {
				fmt.Printf("  %-14s %s\n", "isolation", n)
			}
		}
	}

	// Verify. A cleanup that reports success without checking is worse than
	// none, because it stops you looking.
	fmt.Println("residue check")
	residue := 0
	for _, it := range p.items {
		if _, err := os.Lstat(it.path); err == nil {
			fmt.Printf("  PRESENT  %s (%s)\n", it.what, it.path)
			residue++
		}
	}
	if residue == 0 {
		fmt.Printf("  clean    %d path(s) removed\n", len(p.items))
	}
	// Isolation is checked as well as removed, for the same reason the paths
	// are: it is the residue whose cost is highest and whose absence is
	// least obvious. This used to report "clean" with a job's virtual
	// machine still running.
	if be, err := newLocalBackend(p.root); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), isolationStopWait)
		defer cancel()
		if left := be.SweepOrphanIsolation(ctx, nil); len(left) > 0 {
			for _, n := range left {
				fmt.Printf("  PRESENT  isolation %s (stopped now)\n", n)
			}
			residue++
		} else {
			fmt.Printf("  clean    no isolation left running\n")
		}
	}
	if _, err := os.Lstat(ctl.SocketPath(p.root)); err == nil {
		fmt.Printf("  PRESENT  control socket (%s)\n", ctl.SocketPath(p.root))
		residue++
	} else {
		fmt.Printf("  clean    control socket\n")
	}
	// A daemon still alive here means the removal raced something that is
	// still writing, which the path checks above would not catch.
	if pid, running := daemon.Status(p.root); running {
		fmt.Printf("  PRESENT  running process (pid %d)\n", pid)
		residue++
	} else {
		fmt.Printf("  clean    no shome process\n")
	}

	fmt.Println()
	for _, f := range failed {
		fmt.Printf("  could not remove %s\n", f)
	}
	if residue > 0 {
		return fmt.Errorf("%d item(s) of residue remain", residue)
	}
	fmt.Println("shome removed. Nothing outside those paths was touched.")
	for _, f := range p.profiles {
		fmt.Printf("\nOne thing left, which is yours to edit: %s mentions shome.\n", f)
		fmt.Printf("Remove the PATH line if you added one.\n")
	}
	return nil
}
