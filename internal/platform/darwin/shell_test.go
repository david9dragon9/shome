//go:build darwin

package darwin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/davidwu/shome/internal/platform"
	"github.com/davidwu/shome/internal/userenv"
)

// A logged-in account gets an interactive session that can do real work --
// install packages, run interpreters -- while reaching nothing it should not.
// Both halves are tested against a real sandbox, because an SBPL rule that
// matches nothing fails silently in whichever direction it was written.
func shellFixture(t *testing.T) (*Backend, platform.ShellSpec, string, map[string]string) {
	t.Helper()
	root := sharedTempDir(t)
	ownerHome := t.TempDir()
	ownerHome, _ = filepath.EvalSymlinks(ownerHome)

	secrets := map[string]string{
		filepath.Join(ownerHome, "diary.txt"):            "OWNER-PRIVATE",
		filepath.Join(root, "admin.token"):               "ADMIN-TOKEN",
		filepath.Join(root, "shome.db"):                  "JOB-DATABASE",
		filepath.Join(root, "users", "bob", "notes.txt"): "ANOTHER-ACCOUNT",
	}
	for p, v := range secrets {
		os.MkdirAll(filepath.Dir(p), 0o700)
		if err := os.WriteFile(p, []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	home := filepath.Join(root, "users", "alice")
	if err := userenv.EnsureHome(home); err != nil {
		t.Fatal(err)
	}
	b := &Backend{Root: filepath.Join(root, "local"), OwnerHome: ownerHome, StateRoot: root}
	if _, why := b.container(); why != "" {
		t.Skipf("sessions run in a container on macOS, and it is unavailable: %s", why)
	}
	// The paths the session will see, which is what the environment must be
	// built from -- HOME pointing at a host path would name a directory that
	// does not exist inside.
	layout := b.Layout(platform.Placement{Home: home, User: "alice", Machine: "mini"})
	spec := platform.ShellSpec{
		User: "alice", Home: home, HomeAs: layout.Home,
		AllowNet: true,
		Env: userenv.Session{
			Home: layout.Home, Tools: layout.Tools, Software: layout.Software,
			User: "alice", Cluster: "home", Host: "mini",
		}.Env(),
	}
	return b, spec, root, secrets
}

// run executes one command inside the session and returns its combined output.
func runIn(t *testing.T, b *Backend, spec platform.ShellSpec, script string) (string, error) {
	t.Helper()
	s := spec
	s.Argv = []string{"/bin/sh", "-c", script}
	cmd, err := b.ShellCommand(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestSessionCannotReachAnythingItShouldNot(t *testing.T) {
	b, spec, _, secrets := shellFixture(t)

	for p, v := range secrets {
		out, _ := runIn(t, b, spec, "cat "+p+" 2>&1")
		if strings.Contains(out, v) {
			t.Errorf("a session read %s -- contents %q", p, v)
		}
	}

	// Nor write outside its own directory.
	outside := filepath.Join(filepath.Dir(spec.Home), "bob", "planted")
	runIn(t, b, spec, "echo x > "+outside+" 2>&1")
	if _, err := os.Stat(outside); err == nil {
		t.Error("a session wrote into another account's directory")
	}

	// The stronger property a container gives and a policy cannot: the host
	// is not merely forbidden, it is absent -- so an error message cannot be
	// used to ask whether a file exists.
	existing, _ := runIn(t, b, spec, "ls -d "+filepath.Dir(spec.Home)+" 2>&1")
	missing, _ := runIn(t, b, spec, "ls -d /no/such/path/at/all 2>&1")
	norm := func(s string) string {
		if strings.Contains(s, "No such file") {
			return "absent"
		}
		return strings.TrimSpace(s)
	}
	if norm(existing) != norm(missing) {
		t.Errorf("a host path that exists and one that does not gave different "+
			"answers, so existence can be probed:\n  exists:  %q\n  missing: %q",
			existing, missing)
	}
}

func TestSessionCanUseItsOwnHome(t *testing.T) {
	b, spec, _, _ := shellFixture(t)

	out, err := runIn(t, b, spec, "echo hello > $HOME/f.txt && cat $HOME/f.txt")
	if err != nil || !strings.Contains(out, "hello") {
		t.Fatalf("a session cannot use its own home: %v\n%s", err, out)
	}
	// Subdirectories, which is what a virtualenv is.
	out, err = runIn(t, b, spec, "mkdir -p $HOME/a/b/c && touch $HOME/a/b/c/f && ls $HOME/a/b/c")
	if err != nil || !strings.Contains(out, "f") {
		t.Fatalf("cannot create nested directories: %v\n%s", err, out)
	}
	// A temp file, which countless tools assume works.
	out, err = runIn(t, b, spec, `t=$(mktemp) && echo ok > "$t" && cat "$t"`)
	if err != nil || !strings.Contains(out, "ok") {
		t.Fatalf("mktemp does not work in a session: %v\n%s", err, out)
	}
	// getcwd, which needs the metadata chain above the home.
	// pwd must name the mapped path and say nothing about the host.
	out, err = runIn(t, b, spec, "cd $HOME && pwd")
	if err != nil || !strings.Contains(out, spec.HomeAs) {
		t.Fatalf("pwd = %q, want the mapped path %q: %v", out, spec.HomeAs, err)
	}
	if strings.Contains(out, spec.Home) {
		t.Errorf("pwd leaks where the directory really lives: %q", out)
	}
}

// The environment has to be genuinely usable, which means the package manager
// has to run. uv does enough interesting things -- memory maps, subprocesses,
// its own Python downloads -- that "the profile allows the binary" is not the
// same as "uv works".
func TestUVRunsInsideTheSession(t *testing.T) {
	b, spec, _, _ := shellFixture(t)
	// uv comes from the image: the host's copy is a macOS binary and would
	// not execute in a Linux container.
	out, err := runIn(t, b, spec, "uv --version")
	if err != nil || !strings.Contains(out, "uv ") {
		t.Fatalf("uv does not run in a session: %v\n%s", err, out)
	}

	// The real test: create a virtualenv in the home directory. This writes
	// to the cache, the home, and a nested tree, all inside the sandbox.
	// Not --python-preference only-system: the system python on macOS is an
	// Xcode stub that the sandbox deliberately does not reach, and a session
	// is meant to get its interpreter from uv, into its own directory. That
	// is the whole point of shipping uv.
	out, err = runIn(t, b, spec, "cd $HOME && uv venv 2>&1")
	if err != nil {
		// Fetching an interpreter needs the network, which a machine running
		// tests offline will not have.
		if strings.Contains(out, "Failed to download") || strings.Contains(out, "error sending request") ||
			strings.Contains(out, "No interpreter found") {
			t.Skipf("cannot fetch an interpreter here:\n%s", out)
		}
		t.Fatalf("uv venv failed inside a session: %v\n%s", err, out)
	}
	// pyvenv.cfg rather than .venv/bin/python: the interpreter is a symlink
	// to a path inside the container, which does not resolve when stat'ed
	// from the host. That is inherent to mapping the directory somewhere
	// else, and it is why a venv does not survive being copied to another
	// machine -- venvs are not relocatable in any case.
	if _, err := os.Stat(filepath.Join(spec.Home, ".venv", "pyvenv.cfg")); err != nil {
		t.Fatalf("the venv was not created where the session can keep it: %v\n%s", err, out)
	}

	// And it persists: that is the whole point of using the account's own
	// directory rather than a scratch dir.
	out, err = runIn(t, b, spec, "$HOME/.venv/bin/python -c 'print(1+1)'")
	if err != nil || !strings.Contains(out, "2") {
		t.Fatalf("the venv's python does not run: %v\n%s", err, out)
	}
}

// uv's cache must land inside the account's directory, or it escapes both the
// sandbox and the disk limit and `shome fs` cannot see it.
func TestUVWritesOnlyInsideTheAccountsDirectory(t *testing.T) {
	b, spec, _, _ := shellFixture(t)
	out, err := runIn(t, b, spec, "uv cache dir")
	if err != nil {
		t.Fatalf("uv cache dir: %v\n%s", err, out)
	}
	if !strings.HasPrefix(strings.TrimSpace(out), spec.HomeAs) {
		t.Errorf("uv caches at %q, outside %s", strings.TrimSpace(out), spec.HomeAs)
	}
}

// A session must not be able to inspect or signal the machine owner's
// processes -- shome's own daemon among them.
// A session must not see the machine's processes.
//
// Inside a container the process namespace is its own, so the session sees
// itself and its own init and nothing else -- signalling those is expected
// and means nothing about the host.
func TestSessionCannotSeeOtherProcesses(t *testing.T) {
	b, spec, _, _ := shellFixture(t)
	out, _ := runIn(t, b, spec, "ps -e 2>/dev/null | wc -l")
	t.Logf("processes visible to a session: %s", strings.TrimSpace(out))
	// The agent's own process, and the machine owner's, must not be there.
	out, _ = runIn(t, b, spec, "ps -e 2>/dev/null | grep -c shomed || true")
	if strings.Contains(out, "1") && !strings.Contains(out, "0") {
		t.Errorf("a session can see shome's own daemon: %q", out)
	}
}

func TestShellRefusesWithoutAHome(t *testing.T) {
	b, spec, _, _ := shellFixture(t)
	spec.Home = ""
	if _, err := b.ShellCommand(context.Background(), spec); err == nil {
		t.Error("ShellCommand accepted an empty home")
	}
}

// The session's environment must not carry anything from the daemon that
// started it -- SHOME_ROOT, a token path, the owner's PATH.
func TestSessionEnvironmentIsBuiltFromNothing(t *testing.T) {
	b, spec, _, _ := shellFixture(t)
	t.Setenv("SHOME_TOKEN", "SHOULD-NOT-LEAK")
	t.Setenv("SECRET_OF_THE_DAEMON", "SHOULD-NOT-LEAK")
	out, _ := runIn(t, b, spec, "env")
	if strings.Contains(out, "SHOULD-NOT-LEAK") {
		t.Errorf("the daemon's environment leaked into a session:\n%s", out)
	}
	if !strings.Contains(out, "HOME="+spec.HomeAs) {
		t.Errorf("HOME is not the account's directory as it appears inside:\n%s", out)
	}
}

// sharedTempDir makes a directory the container runtime can actually share.
//
// t.TempDir() lives under /var/folders, which the runtime mounts without
// propagating writes back to the host -- so a test rooted there sees a
// session that appears to work and produces nothing. Production directories
// live under the installation root, which does not have this problem.
func sharedTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/private/tmp", "shome-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return realPath(dir)
}

// A session container is always named, even when the caller forgot.
//
// Killing the launcher does not stop a container, so an unnamed one cannot
// be stopped by anybody: its caller has no handle for it, and the sweep
// cannot tell it from somebody else's work. It holds a virtual machine and
// its memory until the machine is rebooted -- which is what shome's own test
// suite did, once per run.
func TestASessionContainerIsNeverUnnamed(t *testing.T) {
	root := realPath(t.TempDir())
	home := filepath.Join(root, "users", "alice")
	if err := userenv.EnsureHome(home); err != nil {
		t.Fatal(err)
	}
	b := &Backend{Root: filepath.Join(root, "local"), OwnerHome: realPath(t.TempDir()), StateRoot: root}
	if _, why := b.container(); why != "" {
		t.Skipf("sessions run in a container on macOS: %s", why)
	}
	cmd, err := b.ShellCommand(context.Background(), platform.ShellSpec{
		User: "alice", Home: home, HomeAs: "/home/alice",
		Argv: []string{"/bin/true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(cmd.Args, " ")
	if !strings.Contains(args, "--name ") {
		t.Fatalf("no --name in the launch: %s", args)
	}
	// And the name is attributable to this installation, so the sweep can
	// reclaim it rather than leaving it to somebody's next reboot.
	var name string
	for i, a := range cmd.Args {
		if a == "--name" && i+1 < len(cmd.Args) {
			name = cmd.Args[i+1]
		}
	}
	if !strings.HasPrefix(name, b.sessionPrefix()) {
		t.Errorf("generated name %q does not carry this installation's tag (%q)",
			name, b.sessionPrefix())
	}
}
