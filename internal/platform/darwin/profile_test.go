//go:build darwin

package darwin

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// writeProfile renders a profile for a fresh scratch dir and returns both.
func writeProfile(t *testing.T, c ProfileConfig) (profPath, scratch string) {
	t.Helper()
	// t.TempDir() lives under $TMPDIR, which is a symlink -- exactly the case
	// that silently breaks Seatbelt matching, so this exercises realPath too.
	root := t.TempDir()
	scratch = filepath.Join(root, "scratch")
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	c.ScratchDir = scratch
	prof, err := GenerateProfile(c)
	if err != nil {
		t.Fatal(err)
	}
	profPath = filepath.Join(root, "job.sb")
	if err := os.WriteFile(profPath, []byte(prof), 0o600); err != nil {
		t.Fatal(err)
	}
	return profPath, realPath(scratch)
}

// The policy is an allowlist: nothing is readable unless it is named. The
// owner's home is therefore denied by simply not appearing -- and is also
// denied explicitly, so a future widening of the allowlist cannot quietly
// re-expose it.
func TestOwnerHomeIsNotReachable(t *testing.T) {
	p, _ := GenerateProfile(ProfileConfig{ScratchDir: t.TempDir(), OwnerHome: "/Users/someone"})

	// No broad read: that shape is what let a logged-in user browse the
	// owner's applications.
	if strings.Contains(p, "(allow file-read*)\n") {
		t.Errorf("the profile grants an unrestricted file-read*:\n%s", p)
	}
	if !strings.Contains(p, `(deny file-read*  (subpath "/Users/someone"`) {
		t.Errorf("the owner's home is not denied explicitly:\n%s", p)
	}
	// And nothing in the allowlist reaches the places that hold user data.
	for _, forbidden := range []string{
		`(allow file-read* (subpath "/Users`,
		`(allow file-read* (subpath "/Applications`,
		`(allow file-read* (subpath "/Volumes`,
		`(allow file-read* (subpath "/Library`,
	} {
		if strings.Contains(p, forbidden) {
			t.Errorf("the allowlist reaches user data: %s", forbidden)
		}
	}
}

func TestSignalAndProcessInfoRulesAreCorrectlyScoped(t *testing.T) {
	p, _ := GenerateProfile(ProfileConfig{ScratchDir: t.TempDir()})
	denySig := strings.Index(p, "(deny signal)")
	allowSelf := strings.Index(p, "(allow signal (target self))")
	if denySig < 0 || allowSelf < 0 || allowSelf < denySig {
		t.Error("(allow signal (target self)) must follow (deny signal), else the runtime dies")
	}
	if !strings.Contains(p, "(deny process-info* (target others))") {
		t.Error("process-info* deny must be scoped to (target others)")
	}
	if strings.Contains(p, "(deny process-info*)\n") {
		t.Error("unscoped (deny process-info*) aborts the runtime with exit 133")
	}
}

func TestGPUDirectivesOnlyWhenRequested(t *testing.T) {
	for _, gpu := range []bool{false, true} {
		p, _ := GenerateProfile(ProfileConfig{ScratchDir: t.TempDir(), AllowGPU: gpu})
		has := strings.Contains(p, "AGXDeviceUserClient") && strings.Contains(p, "MTLCompilerService")
		if has != gpu {
			t.Errorf("AllowGPU=%v: GPU directives present=%v", gpu, has)
		}
	}
}

func TestPathsAreResolvedAndEscaped(t *testing.T) {
	// /tmp is a symlink to /private/tmp; an unresolved path matches nothing.
	p, _ := GenerateProfile(ProfileConfig{ScratchDir: "/tmp"})
	if !strings.Contains(p, `"/private/tmp"`) {
		t.Errorf("scratch path not resolved to its real path:\n%s", p)
	}
	// A quote in a path must not terminate the SBPL string literal.
	if got := sbplEscape(`/x/"; (allow default) ;"`); strings.Contains(got, `";`) && !strings.Contains(got, `\"`) {
		t.Errorf("quote not escaped: %s", got)
	}
}

// --- integration: the policy must actually hold when enforced by the kernel ---

func TestSandboxActuallyEnforces(t *testing.T) {
	if _, err := os.Stat("/usr/bin/sandbox-exec"); err != nil {
		t.Skip("sandbox-exec unavailable")
	}
	home := t.TempDir() // stand-in for the owner's home
	secret := filepath.Join(home, "secret.txt")
	if err := os.WriteFile(secret, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir() // stand-in for another job's scratch
	otherFile := filepath.Join(other, "theirs.txt")
	if err := os.WriteFile(otherFile, []byte("theirs"), 0o600); err != nil {
		t.Fatal(err)
	}

	prof, scratch := writeProfile(t, ProfileConfig{
		OwnerHome: home,
		DenyPaths: []string{other},
		AllowGPU:  true,
	})

	// Preflight: if a trivial command cannot run at all, every deny-assertion
	// below would pass for the wrong reason.
	if err := exec.Command("/usr/bin/sandbox-exec", "-f", prof, "/usr/bin/true").Run(); err != nil {
		t.Fatalf("sandboxed process cannot start; deny-assertions would be meaningless: %v", err)
	}

	run := func(args ...string) error {
		return exec.Command("/usr/bin/sandbox-exec", append([]string{"-f", prof}, args...)...).Run()
	}

	t.Run("owner home unreadable", func(t *testing.T) {
		if err := run("/bin/cat", secret); err == nil {
			t.Error("read of owner's home succeeded; it must be denied")
		}
	})
	t.Run("owner home unwritable", func(t *testing.T) {
		if err := run("/usr/bin/touch", filepath.Join(home, "evil")); err == nil {
			t.Error("write into owner's home succeeded; it must be denied")
		}
		if _, err := os.Stat(filepath.Join(home, "evil")); err == nil {
			t.Error("file was actually created in the owner's home")
		}
	})
	t.Run("other job unreadable", func(t *testing.T) {
		if err := run("/bin/cat", otherFile); err == nil {
			t.Error("cross-job read succeeded; it must be denied")
		}
	})
	t.Run("other job unwritable", func(t *testing.T) {
		_ = run("/usr/bin/touch", filepath.Join(other, "evil"))
		if _, err := os.Stat(filepath.Join(other, "evil")); err == nil {
			t.Error("cross-job write actually created a file")
		}
	})
	t.Run("own scratch writable", func(t *testing.T) {
		if err := run("/usr/bin/touch", filepath.Join(scratch, "ok")); err != nil {
			t.Errorf("job cannot write its own scratch: %v", err)
		}
	})
	t.Run("cannot enumerate other processes", func(t *testing.T) {
		out, _ := exec.Command("/usr/bin/sandbox-exec", "-f", prof, "/bin/ps", "-ax").Output()
		if n := len(strings.Fields(strings.TrimSpace(string(out)))); n != 0 {
			t.Errorf("ps returned %d fields; expected none", n)
		}
	})
	t.Run("network denied by default", func(t *testing.T) {
		// nc exits non-zero when the connect is blocked.
		if err := run("/usr/bin/nc", "-z", "-G", "2", "1.1.1.1", "80"); err == nil {
			t.Error("outbound connection succeeded; network must be deny-by-default")
		}
	})
}

// Regression: a job's scratch normally lives inside the denied jobs root, so
// the scratch allow must be emitted AFTER those denies or the job cannot reach
// its own working directory.
// The writable area must be readable and writable, and its ancestors
// stat-able so getcwd(3) works -- those ancestors are not themselves
// readable.
func TestScratchIsReadWriteAndReachable(t *testing.T) {
	dir := t.TempDir()
	p, err := GenerateProfile(ProfileConfig{ScratchDir: dir, OwnerHome: "/Users/someone"})
	if err != nil {
		t.Fatal(err)
	}
	real := realPath(dir)
	for _, want := range []string{
		`(allow file-read*  (subpath "` + real + `")`,
		`(allow file-write* (subpath "` + real + `")`,
		`(allow file-read-metadata (literal "/")`,
	} {
		if !strings.Contains(p, want) {
			t.Errorf("missing expected rule %q in:\n%s", want, p)
		}
	}
	// Every ancestor is stat-able, or getcwd fails inside the sandbox.
	for _, a := range ancestors(real) {
		if !strings.Contains(p, `(allow file-read-metadata (literal "`+a+`"))`) {
			t.Errorf("ancestor %q is not stat-able, so getcwd would fail", a)
		}
	}
}

// And prove it end-to-end: a job must be able to use its own scratch as cwd
// while a sibling job's scratch stays unreachable.
func TestScratchUsableWhileSiblingsDenied(t *testing.T) {
	root := t.TempDir()
	jobs := filepath.Join(root, "jobs")
	mine := filepath.Join(jobs, "job-1")
	theirs := filepath.Join(jobs, "job-2")
	for _, d := range []string{mine, theirs} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(theirs, "s.txt"), []byte("theirs"), 0o600); err != nil {
		t.Fatal(err)
	}
	prof, _ := GenerateProfile(ProfileConfig{ScratchDir: mine, OwnerHome: t.TempDir(), DenyPaths: []string{jobs}})
	pf := filepath.Join(root, "j.sb")
	if err := os.WriteFile(pf, []byte(prof), 0o600); err != nil {
		t.Fatal(err)
	}

	c := exec.Command("/usr/bin/sandbox-exec", "-f", pf, "/bin/sh", "-c", "pwd >/dev/null && echo ok > mine.txt && cat mine.txt")
	c.Dir = realPath(mine)
	out, err := c.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "ok") {
		t.Errorf("job cannot use its own scratch as cwd: err=%v out=%q", err, out)
	}
	c2 := exec.Command("/usr/bin/sandbox-exec", "-f", pf, "/bin/cat", filepath.Join(theirs, "s.txt"))
	c2.Dir = realPath(mine)
	if out2, err := c2.CombinedOutput(); err == nil || strings.Contains(string(out2), "theirs") {
		t.Errorf("sibling job's scratch was readable: %q", out2)
	}
}

// Regression: a state root reached through a symlink -- /tmp, or any
// directory an admin symlinked -- must still be usable by the job.
//
// Seatbelt matches the resolved path, so the profile's rules named
// /private/tmp/... while the job's own paths said /tmp/..., and reading the
// link to get from one to the other was itself denied. The job could not
// read the script it was about to run, and the only diagnostic was
// "Operation not permitted".
func TestScratchReachableThroughSymlink(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	jobs := filepath.Join(real, "jobs")
	if err := os.MkdirAll(filepath.Join(jobs, "job-1"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	// The spelling the agent would use: through the link, unresolved.
	scratch := filepath.Join(link, "jobs", "job-1")
	prof, err := GenerateProfile(ProfileConfig{
		ScratchDir: scratch,
		OwnerHome:  t.TempDir(),
		DenyPaths:  []string{filepath.Join(link, "jobs")},
	})
	if err != nil {
		t.Fatal(err)
	}
	pf := filepath.Join(root, "j.sb")
	if err := os.WriteFile(pf, []byte(prof), 0o600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(scratch, "job-script")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho ran\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	// Every path here is the symlinked spelling, which is what the job sees.
	c := exec.Command("/usr/bin/sandbox-exec", "-f", pf, "/bin/sh", script)
	c.Dir = scratch
	out, err := c.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "ran") {
		t.Errorf("job script under a symlinked root did not run: err=%v out=%q", err, out)
	}
}

// A job must be able to fork a worker pool and ask who it is running as.
//
// Both were denied, and both failed in a way that named nothing: a bare
// EPERM from multiprocessing, and a KeyError from getpwuid. Each is a POSIX
// facility that anything nontrivial assumes, so this is about the sandbox
// being usable rather than about any one library.
func TestJobsCanUseIPCAndResolveTheirAccount(t *testing.T) {
	p, err := GenerateProfile(ProfileConfig{ScratchDir: t.TempDir(), OwnerHome: "/Users/someone"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"(allow ipc-posix-sem)",
		"(allow ipc-posix-shm)",
		`(allow mach-lookup (global-name "com.apple.system.opendirectoryd.libinfo"))`,
		`(subpath "/private/var/db/timezone")`,
	} {
		if !strings.Contains(p, want) {
			t.Errorf("missing %q", want)
		}
	}
}

// The Metal allowances, including the one a hand-written compute shader does
// not need and every real framework does.
func TestGPUProfileAllowsSharedEvents(t *testing.T) {
	with, err := GenerateProfile(ProfileConfig{
		ScratchDir: t.TempDir(), OwnerHome: "/Users/someone", AllowGPU: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`(iokit-user-client-class "AGXDeviceUserClient")`,
		`(iokit-user-client-class "IOSurfaceRootUserClient")`,
		`(global-name "com.apple.MTLCompilerService")`,
	} {
		if !strings.Contains(with, want) {
			t.Errorf("a GPU job's profile is missing %q", want)
		}
	}
	// And none of it for a job that did not ask for a GPU.
	without, err := GenerateProfile(ProfileConfig{
		ScratchDir: t.TempDir(), OwnerHome: "/Users/someone"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(without, "iokit-open") {
		t.Error("a job that asked for no GPU was given the Metal allowances")
	}
}

// A job may use a unix socket inside its own directory without asking for
// the network, and that must not have opened the network.
//
// Seatbelt counts binding a unix socket as a network operation, so a job
// with no --network could not talk to its own worker processes. The rule is
// filtered by path, which is what keeps the two apart.
func TestUnixSocketsAreAllowedButIPIsNot(t *testing.T) {
	// Not t.TempDir(): a unix socket path has 104 bytes to play with and
	// the per-test temp directory alone is longer than that.
	scratch, err := os.MkdirTemp("/tmp", "shome-sock")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(scratch) })
	prof, err := GenerateProfile(ProfileConfig{ScratchDir: scratch, OwnerHome: "/Users/someone"})
	if err != nil {
		t.Fatal(err)
	}
	real := realPath(scratch)
	for _, want := range []string{
		`(allow network-bind     (subpath "` + real + `"))`,
		`(allow network-outbound (subpath "` + real + `"))`,
	} {
		if !strings.Contains(prof, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(prof, "(allow network*)") {
		t.Fatal("the network was allowed outright for a job that did not ask")
	}

	pf := filepath.Join(scratch, "p.sb")
	if err := os.WriteFile(pf, []byte(prof), 0o600); err != nil {
		t.Fatal(err)
	}
	// A socket in the job's own directory: bind, connect, exchange a byte.
	// Driven with nc, whose -U speaks unix sockets, because it is in
	// /usr/bin and needs nothing the profile does not already allow.
	script := `set -e
(printf x | /usr/bin/nc -lU "$S/s.sock") & sleep 0.5
got=$(/usr/bin/nc -U "$S/s.sock" | head -c 1)
[ "$got" = x ] && echo "unix socket ok"`
	c := exec.Command("/usr/bin/sandbox-exec", "-f", pf, "/bin/sh", "-c", script)
	c.Dir = real
	c.Env = []string{"S=" + real, "HOME=" + real, "PATH=/usr/bin:/bin", "TMPDIR=" + real}
	if out, err := c.CombinedOutput(); err != nil || !strings.Contains(string(out), "unix socket ok") {
		t.Errorf("a unix socket in the job's own directory was refused: err=%v out=%s", err, out)
	}
	// And the network is still shut.
	c2 := exec.Command("/usr/bin/sandbox-exec", "-f", pf, "/usr/bin/nc", "-z", "-G", "2", "1.1.1.1", "80")
	c2.Dir = real
	if err := c2.Run(); err == nil {
		t.Error("outbound IP succeeded; the path filter must not open the network")
	}
}
