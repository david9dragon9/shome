//go:build darwin

package darwin

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/davidwu/shome/internal/job"
	"github.com/davidwu/shome/internal/platform"
	"github.com/davidwu/shome/internal/userenv"
)

// The property a user asked for after finding it absent: a logged-in account
// can see its own directory and the OS's program files, and nothing else.
//
// The profile used to grant a broad (allow file-read*) and subtract from it,
// on a recorded finding that an allowlist "aborts dyld". The finding was
// wrong -- the allowlist was missing read access to the root directory, which
// dyld stats on every exec -- and the cost of being wrong was that anyone
// with an account could browse /Applications, /Users and the owner's files.
//
// Run against a real sandbox: an SBPL rule that matches nothing fails
// silently, so nothing short of executing something proves this.
func TestSessionSeesOnlyItsOwnDirectory(t *testing.T) {
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

	if _, why := b.container(); why != "" {
		t.Skipf("sessions run in a container on macOS: %s", why)
	}
	layout := b.Layout(platform.Placement{Home: home, User: "alice", Machine: "mini"})
	run := func(script string) string {
		s := platform.ShellSpec{
			User: "alice", Home: home, HomeAs: layout.Home,
			Argv: []string{"/bin/sh", "-c", script},
			Env:  map[string]string{"HOME": layout.Home},
		}
		cmd, err := b.ShellCommand(context.Background(), s)
		if err != nil {
			t.Fatal(err)
		}
		out, _ := cmd.CombinedOutput()
		return string(out)
	}

	// Nothing that belongs to a person is reachable. /private/var/folders is
	// the per-user temp area; /usr/local is where third-party software goes
	// and names what the owner has installed.
	// /opt and /usr/local exist inside a Linux container as its own empty
	// directories; naming them here would be testing the image, not the
	// isolation. These are the paths that would hold something of the
	// host's if any of it leaked through.
	for _, p := range []string{
		"/Applications", "/Users", "/Volumes", "/Library",
		"/private/tmp", "/private/var/log", "/private/var/folders",
		ownerHome,
	} {
		// Listing the contents, not stat-ing the path. A directory on the
		// way to the account's own home has to be stat-able or getcwd(3)
		// fails inside the sandbox -- that reveals only that a path exists,
		// which the account already knows. Reading what is in it is the
		// thing that must not work.
		out := run("ls " + p + " >/dev/null 2>&1 && echo REACHABLE || echo denied")
		if strings.Contains(out, "REACHABLE") {
			t.Errorf("%s can be listed from a session", p)
		}
	}
	if out := run("cat " + filepath.Join(ownerHome, "diary")); strings.Contains(out, "PRIVATE") {
		t.Error("a session read the owner's file")
	}

	// Listing the root shows the standard top-level names -- every Mac has
	// them and they hold nothing of the owner's -- but nothing below.
	if out := run("cat /Applications/* 2>/dev/null | head -c 20"); strings.TrimSpace(out) != "" {
		t.Errorf("something under /Applications was readable: %q", out)
	}

	// And the session is still usable.
	if out := run("echo hi > $HOME/f && cat $HOME/f"); !strings.Contains(out, "hi") {
		t.Errorf("a session cannot use its own directory: %q", out)
	}
	// pwd names the mapped path and says nothing about the host.
	out := run("cd $HOME && pwd")
	if !strings.Contains(out, layout.Home) {
		t.Errorf("pwd = %q, want the mapped path %q", out, layout.Home)
	}
	if strings.Contains(out, home) {
		t.Errorf("pwd leaks where the directory really lives: %q", out)
	}
	for _, tool := range []string{"/bin/sh", "/bin/ls", "/usr/bin/env"} {
		if out := run(tool + " --version >/dev/null 2>&1; " + tool + " >/dev/null 2>&1; echo ran"); !strings.Contains(out, "ran") {
			t.Errorf("%s does not run in a session: %q", tool, out)
		}
	}
}

// A batch job gets the same treatment: its scratch, and the OS. The job path
// and the session path share one profile generator so they cannot diverge.
func TestBatchJobSeesOnlyItsScratch(t *testing.T) {
	root := sharedTempDir(t)
	ownerHome := realPath(t.TempDir())
	os.WriteFile(filepath.Join(ownerHome, "diary"), []byte("PRIVATE"), 0o600)
	os.MkdirAll(filepath.Join(root, "users", "someone"), 0o700)
	os.WriteFile(filepath.Join(root, "admin.token"), []byte("ADMIN-TOKEN"), 0o600)

	b := &Backend{Root: filepath.Join(root, "local"), OwnerHome: ownerHome, StateRoot: root}
	sb, err := b.Prepare(context.Background(), job.Spec{Name: "j"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Cleanup(context.Background(), sb) })

	run := func(script string) string {
		out, _ := exec.Command("sandbox-exec", "-f", sb.ProfilePath,
			"/bin/sh", "-c", script).CombinedOutput()
		return string(out)
	}
	for _, p := range []string{"/Applications", "/Users", ownerHome,
		filepath.Join(root, "users")} {
		if out := run("ls -d " + p + " >/dev/null 2>&1 && echo REACHABLE"); strings.Contains(out, "REACHABLE") {
			t.Errorf("a job can reach %s", p)
		}
	}
	// /Library is a special case, and the distinction is the whole point of
	// how the software prefixes are opened. A job that runs natively is
	// allowed to read one tree inside it -- the Command Line Tools, which is
	// where git comes from -- and path resolution walks every component, so
	// /Library itself has to be stat-able. That grants nothing: its contents
	// must still not be listable, and nothing else under it readable.
	if out := run("ls /Library"); !strings.Contains(out, "not permitted") {
		t.Errorf("a job can list /Library: %q", out)
	}
	if out := run("ls /Library/Developer"); !strings.Contains(out, "not permitted") {
		t.Errorf("a job can list /Library/Developer, so it can see what "+
			"developer tools the owner has: %q", out)
	}
	if out := run("cat /Library/Preferences/.GlobalPreferences.plist"); !strings.Contains(out, "not permitted") {
		t.Errorf("a job read the machine's preferences: %q", out)
	}
	if out := run("cat " + filepath.Join(root, "admin.token")); strings.Contains(out, "ADMIN-TOKEN") {
		t.Error("a job read the cluster's admin token")
	}
	// Still works.
	if out := run("cd " + sb.ScratchDir + " && echo ok > f && cat f"); !strings.Contains(out, "ok") {
		t.Errorf("a job cannot use its scratch: %q", out)
	}
}

// The allowlist must not name anything that holds a person's data. A cheap
// check on the generated text, so a careless addition is caught without
// having to run it.
func TestAllowlistNamesNothingPersonal(t *testing.T) {
	scratch := t.TempDir()
	p, err := GenerateProfile(ProfileConfig{ScratchDir: scratch})
	if err != nil {
		t.Fatal(err)
	}
	// The writable area is exempt: it is the one place the job may read and
	// write, and on macOS a test's temp dir always lives under
	// /private/var/folders, which the scan below treats as personal.
	real := realPath(scratch)
	for _, bad := range []string{
		"/Applications", "/Users", "/Volumes", "/usr/local",
		"/private/var/folders", "/private/tmp",
	} {
		// Read rules only. Metadata rules for the ancestors of the writable
		// area are required for getcwd(3) and grant no contents.
		for _, line := range strings.Split(p, "\n") {
			if !strings.HasPrefix(line, "(allow file-read* ") || strings.Contains(line, real) {
				continue
			}
			if strings.Contains(line, `(subpath "`+bad) || strings.Contains(line, `(literal "`+bad) {
				t.Errorf("the allowlist grants read on %s, which holds user data: %s", bad, line)
			}
		}
	}
	// /Library only ever as metadata, never readable.
	if strings.Contains(p, `(allow file-read* (subpath "/Library`) {
		t.Error("the allowlist grants read on /Library")
	}
	// And the shape itself: no unrestricted read.
	if strings.Contains(p, "(allow file-read*)\n") {
		t.Error("the profile grants an unrestricted file-read*")
	}
}

// macOS puts its own state in $HOME given the chance, and a natively-run job
// -- a GPU job -- has HOME pointing at the account's portable storage. What
// lands there cannot travel to another machine, counts against the account's
// disk quota, and shows up in `shome fs ls` as though the account had put it
// there. So the account's work is writable and its ~/Library is not.
func TestNativeJobCannotWriteMacOSLibraryIntoTheAccountsHome(t *testing.T) {
	root := sharedTempDir(t)
	home := filepath.Join(root, "users", "alice")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	b := &Backend{Root: filepath.Join(root, "local"), OwnerHome: realPath(t.TempDir()), StateRoot: root}
	sb, err := b.Prepare(context.Background(), job.Spec{Name: "j", User: "alice"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Cleanup(context.Background(), sb) })

	run := func(script string) string {
		out, _ := exec.Command("sandbox-exec", "-f", sb.ProfilePath,
			"/bin/sh", "-c", script).CombinedOutput()
		return string(out)
	}
	if out := run("mkdir -p " + filepath.Join(home, "Library", "Caches") + " && echo MADE"); strings.Contains(out, "MADE") {
		t.Errorf("a job created ~/Library in the account's storage: %q", out)
	}
	if _, err := os.Stat(filepath.Join(home, "Library")); err == nil {
		t.Error("~/Library exists in the account's storage after a job ran")
	}
	// The account's own work is still writable, which is the whole reason
	// the home is in the profile at all.
	if out := run("echo ok > " + filepath.Join(home, "result.txt") + " && cat " + filepath.Join(home, "result.txt")); !strings.Contains(out, "ok") {
		t.Errorf("a job cannot write to the account's own storage: %q", out)
	}
	// Readable, so an account that collected one before this rule existed
	// can still see it.
	if err := os.MkdirAll(filepath.Join(home, "Library", "Caches"), 0o700); err != nil {
		t.Fatal(err)
	}
	if out := run("ls " + filepath.Join(home, "Library")); !strings.Contains(out, "Caches") {
		t.Errorf("an existing ~/Library is not readable: %q", out)
	}
}

// The Command Line Tools python is what created ~/Library in practice: it
// caches bytecode under ~/Library/Caches/com.apple.python the first time it
// imports anything. Denying that must not stop it running -- a cache that
// cannot be written is simply not used.
func TestCommandLineToolsPythonRunsWithoutItsLibraryCache(t *testing.T) {
	const py = "/Library/Developer/CommandLineTools/usr/bin/python3"
	if _, err := os.Stat(py); err != nil {
		t.Skipf("no Command Line Tools python on this machine: %v", err)
	}
	root := sharedTempDir(t)
	home := filepath.Join(root, "users", "alice")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	b := &Backend{Root: filepath.Join(root, "local"), OwnerHome: realPath(t.TempDir()), StateRoot: root}
	sb, err := b.Prepare(context.Background(), job.Spec{Name: "j", User: "alice"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Cleanup(context.Background(), sb) })

	script := filepath.Join(home, "hello.py")
	if err := os.WriteFile(script, []byte("import json, platform\nprint('py ok', json.dumps({'m': platform.machine()}))\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sandbox-exec", "-f", sb.ProfilePath, py, script)
	cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin", "TMPDIR=" + sb.ScratchDir}
	out, _ := cmd.CombinedOutput()
	if !strings.Contains(string(out), "py ok") {
		t.Errorf("python does not run under the profile: %q", out)
	}
	if _, err := os.Stat(filepath.Join(home, "Library")); err == nil {
		t.Error("python still put a cache in the account's storage")
	}
}
