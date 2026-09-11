//go:build darwin

package darwin

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/davidwu/shome/internal/job"
	"github.com/davidwu/shome/internal/platform"
	"github.com/davidwu/shome/internal/userenv"
)

// A GPU job cannot run in a container -- Metal does not exist inside a
// virtual machine -- so it is the one job that runs on the machine's own
// filesystem, and for a long time that meant it had no software at all: no
// git, no editor, not even shome's own commands. The container path had an
// image and the native path had nothing.
//
// It now reaches the machine's own copies instead. This test runs the real
// thing, because every part of it fails silently otherwise: an SBPL rule
// that matches nothing is not an error, a PATH entry that does not exist is
// not an error, and macOS's /usr/bin/git reports a version perfectly well
// before failing on the first operation that needs the binary it forwards
// to.
func TestGPUJobHasTheBaseSoftware(t *testing.T) {
	if sandboxExecPath == "" {
		t.Skip("no sandbox-exec on this machine")
	}
	root := sharedTempDir(t)
	ownerHome := realPath(t.TempDir())
	if err := os.WriteFile(filepath.Join(ownerHome, "diary"), []byte("PRIVATE"), 0o600); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "users", "alice")
	if err := userenv.EnsureHome(home); err != nil {
		t.Fatal(err)
	}
	b := &Backend{Root: filepath.Join(root, "local"), OwnerHome: ownerHome, StateRoot: root}

	// A GPU job, which is what forces the native path.
	spec := job.Spec{User: "alice", Limits: job.Limits{GPUs: 1}}
	sb, err := b.Prepare(t.Context(), spec, 1)
	if err != nil {
		t.Fatal(err)
	}
	sb.UserHome = home

	// Composed exactly as the agent composes it, so this covers the wiring
	// and not just the profile.
	lay := b.Layout(platform.Placement{
		Home: home, Tools: userenv.ToolDir(root),
		User: "alice", Machine: "mini", GPUs: spec.Limits.GPUs,
	})
	if lay.Mapped {
		t.Fatal("a GPU job was given a container's layout; it does not run in one")
	}
	env := userenv.Session{
		Home: lay.Home, Tools: lay.Tools, Software: lay.Software,
		User: "alice", Cluster: "home", Host: "mini",
	}.Env()
	env["HOME"] = lay.Home
	env["TMPDIR"] = sb.ScratchDir

	run := func(script string) (string, error) {
		cmd := exec.Command(sandboxExecPath, "-f", sb.ProfilePath, "/bin/sh", "-c", script)
		cmd.Dir = sb.ScratchDir
		for k, v := range env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	// git is the one that matters and the one that was reported missing.
	// Skipped rather than failed where the machine genuinely has no copy:
	// shome does not install software onto a machine someone has lent it,
	// and reporting that honestly is the designed behaviour.
	var haveGit bool
	for _, tool := range userenv.NativeStatus() {
		if tool.Command == "git" && tool.Path != "" {
			haveGit = true
		}
	}
	if !haveGit {
		t.Skip("this machine has no git outside the developer stub")
	}

	// Not just --version: the stub in /usr/bin answers that without ever
	// touching the binary it forwards to, so a version alone proves nothing.
	out, err := run(`git init -q repo && cd repo &&
		git -c user.email=t@example.invalid -c user.name=t commit -q --allow-empty -m x &&
		git log --oneline | wc -l`)
	if err != nil {
		t.Fatalf("git could not create a repository in a GPU job: %v\n%s", err, out)
	}
	if strings.TrimSpace(out) != "1" {
		t.Errorf("git log printed %q, want one commit", strings.TrimSpace(out))
	}

	// The whole point of opening those trees is the software. It must not
	// have opened anything else along the way.
	if out, _ := run("cat " + filepath.Join(ownerHome, "diary")); strings.Contains(out, "PRIVATE") {
		t.Errorf("a GPU job read the machine owner's file: %s", out)
	}
	for _, p := range []string{"/Users", "/Applications", "/Volumes"} {
		if out, _ := run("ls " + p); strings.TrimSpace(out) != "" &&
			!strings.Contains(out, "not permitted") && !strings.Contains(out, "Operation not") {
			t.Errorf("a GPU job listed %s: %s", p, out)
		}
	}
}

// shome's own commands have to be there too. A job that cannot run squeue
// is a job that cannot see the cluster it is part of, and the managed tool
// directory lives inside the installation root that the profile otherwise
// denies whole -- so it is only reachable if it was carved back out.
func TestNativeJobCanRunShomesOwnCommands(t *testing.T) {
	if sandboxExecPath == "" {
		t.Skip("no sandbox-exec on this machine")
	}
	root := sharedTempDir(t)
	home := filepath.Join(root, "users", "alice")
	if err := userenv.EnsureHome(home); err != nil {
		t.Fatal(err)
	}
	// A stand-in for the real binary: this is about whether the directory is
	// reachable and on PATH, not about what shome prints.
	tools := userenv.ToolDir(root)
	if err := os.MkdirAll(tools, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tools, "squeue"),
		[]byte("#!/bin/sh\necho JOBID\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	b := &Backend{Root: filepath.Join(root, "local"),
		OwnerHome: realPath(t.TempDir()), StateRoot: root}

	spec := job.Spec{User: "alice", Limits: job.Limits{GPUs: 1}}
	sb, err := b.Prepare(t.Context(), spec, 1)
	if err != nil {
		t.Fatal(err)
	}
	lay := b.Layout(platform.Placement{
		Home: home, Tools: tools, User: "alice", Machine: "mini", GPUs: 1,
	})
	env := userenv.Session{
		Home: lay.Home, Tools: lay.Tools, Software: lay.Software,
		User: "alice", Cluster: "home", Host: "mini",
	}.Env()
	env["HOME"] = lay.Home

	cmd := exec.Command(sandboxExecPath, "-f", sb.ProfilePath, "/bin/sh", "-c", "squeue")
	cmd.Dir = sb.ScratchDir
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "JOBID") {
		t.Fatalf("a native job could not run shome's own commands: %v\n%s", err, out)
	}
}

// Opening the machine's software prefixes must open nothing else. The read
// allowances a job's profile carries are enumerable, so enumerate them: a
// prefix added carelessly later shows up here rather than in somebody's
// home directory.
func TestAJobsReadableTreesAreOnlyTheOnesIntended(t *testing.T) {
	root := sharedTempDir(t)
	home := filepath.Join(root, "users", "alice")
	if err := userenv.EnsureHome(home); err != nil {
		t.Fatal(err)
	}
	b := &Backend{Root: filepath.Join(root, "local"),
		OwnerHome: realPath(t.TempDir()), StateRoot: root}
	sb, err := b.Prepare(t.Context(), job.Spec{User: "alice", Limits: job.Limits{GPUs: 1}}, 1)
	if err != nil {
		t.Fatal(err)
	}
	prof, err := os.ReadFile(sb.ProfilePath)
	if err != nil {
		t.Fatal(err)
	}

	allowed := map[string]bool{
		realPath(sb.ScratchDir):     true,
		realPath(home):              true,
		realPath(userenv.Dir(root)): true,
		"/usr/lib":                  true,
		"/usr/share":                true,
		"/usr/bin":                  true,
		"/usr/libexec":              true,
		"/usr/sbin":                 true,
		"/System":                   true,
		"/bin":                      true,
		"/sbin":                     true,
		"/private/var/select":       true,
		"/var/select":               true,
		"/private/etc/ssl":          true,
		"/private/etc/pam.d":        true,
		"/private/var/db/timezone":  true,
		"/dev/fd":                   true,
	}
	for _, r := range userenv.SoftwareRoots(userenv.MachineSoftware()) {
		allowed[realPath(r)] = true
	}
	for _, line := range strings.Split(string(prof), "\n") {
		const pre = `(allow file-read* (subpath "`
		if !strings.HasPrefix(line, pre) {
			continue
		}
		p := strings.TrimSuffix(strings.TrimPrefix(line, pre), `"))`)
		if !allowed[p] {
			t.Errorf("a job's profile allows reading %q, which is not the "+
				"scratch, the account's storage, shome's own directory, the "+
				"OS, or a software prefix this machine provides", p)
		}
	}
}
