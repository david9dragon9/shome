//go:build darwin

package darwin

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/davidwu/shome/internal/job"
	"github.com/davidwu/shome/internal/platform"
	"github.com/davidwu/shome/internal/ptyx"
	"github.com/davidwu/shome/internal/userenv"
)

// A sandboxed interactive shell must behave like a terminal, which means
// readline has to be able to turn the terminal's echo off.
//
// Regression, and one a user hit: the profile is deny-by-default and had no
// file-ioctl rule at all, so tcsetattr failed. readline could not disable
// ECHO, so the tty driver echoed each line as it was typed and readline
// echoed it again -- every command appeared twice. Line editing was broken
// for the same reason.
//
// Tested against a real sandbox and a real pty because an SBPL rule that
// matches nothing fails silently, and because the symptom only appears when
// something is actually reading a terminal.
func TestSandboxedShellEchoesOnce(t *testing.T) {
	if _, err := os.Stat("/bin/bash"); err != nil {
		t.Skip("no bash to drive readline")
	}
	root := t.TempDir()
	root = realPath(root)
	home := root + "/users/alice"
	if err := userenv.EnsureHome(home); err != nil {
		t.Fatal(err)
	}
	b := &Backend{Root: root + "/local", OwnerHome: t.TempDir(), StateRoot: root}

	// The terminal exists before the policy, because the policy has to name
	// it. Getting that order wrong is the bug.
	p, err := ptyx.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	p.Resize(24, 80, 0, 0)

	// Named, and stopped at the end. Killing the launcher does not stop a
	// container: this test left one running -- a virtual machine holding a
	// gigabyte -- on every run of the suite, twenty-one of them before
	// anybody looked at `container list`.
	name := b.sessionPrefix() + "tty-test"
	cmd, err := b.ShellCommand(context.Background(), platform.ShellSpec{
		User: "alice", Home: home, AllowNet: true,
		Argv:    []string{"/bin/bash", "--norc", "-i"},
		Env:     map[string]string{"PS1": "READY$ ", "TERM": "xterm-256color"},
		TTY:     true,
		TTYPath: p.Slave.Name(),
		Name:    name,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.StopIsolation(context.Background(), name) })
	p.Attach(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p.CloseSlave()
	defer func() { cmd.Process.Kill(); cmd.Wait() }()

	// Drained continuously in the background. Read deadlines are not
	// supported on every device, so a Read with nothing to return can block
	// forever -- which hung this test before.
	var mu sync.Mutex
	var buf strings.Builder
	go func() {
		b := make([]byte, 4096)
		for {
			n, err := p.Master.Read(b)
			if n > 0 {
				mu.Lock()
				buf.Write(b[:n])
				mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	since := func() string {
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}
	reset := func() int {
		mu.Lock()
		defer mu.Unlock()
		return buf.Len()
	}

	// Wait for the shell to be ready rather than sleeping a fixed time.
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(since(), "READY") && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(since(), "READY") {
		t.Fatalf("the sandboxed shell never reached a prompt:\n%q", since())
	}
	mark := reset()

	// Typed a character at a time, as a person does: the doubling only shows
	// up when the terminal is echoing alongside readline.
	const line = "echo MARKER"
	for _, c := range []byte(line + "\r") {
		p.Master.Write([]byte{c})
		time.Sleep(20 * time.Millisecond)
	}
	// Wait for the command's own output, so the assertion is not racing it.
	deadline = time.Now().Add(10 * time.Second)
	for !strings.Contains(since()[mark:], "MARKER\r\n") && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	got := since()[mark:]

	if n := strings.Count(got, line); n != 1 {
		t.Errorf("the typed command came back %d times, want once:\n%q\n\n"+
			"Two means the sandbox is denying tcsetattr, so readline cannot "+
			"turn off the terminal's echo.", n, got)
	}
	if !strings.Contains(got, "MARKER") {
		t.Errorf("the command did not run:\n%q", got)
	}
}

// The rules must name one terminal, not a pattern. The profile grants a broad
// file-read*, so a pattern over /dev/ttys* would let a job open and ioctl a
// terminal belonging to the machine's owner -- and injecting keystrokes into
// somebody else's shell is an escape, not an inconvenience.
func TestTTYRulesNameOneTerminal(t *testing.T) {
	rules := TTYRules("/dev/ttys042")
	if !strings.Contains(rules, `(literal "/dev/ttys042")`) {
		t.Errorf("rules do not name the terminal:\n%s", rules)
	}
	if !strings.Contains(rules, `(literal "/dev/tty")`) {
		t.Errorf("rules omit the process's own controlling terminal:\n%s", rules)
	}
	if strings.Contains(rules, "regex") || strings.Contains(rules, "subpath") {
		t.Errorf("rules use a pattern, which would reach other terminals:\n%s", rules)
	}
	// And nothing beyond ioctl.
	for _, forbidden := range []string{"file-write", "file-read", "process-exec"} {
		if strings.Contains(rules, forbidden) {
			t.Errorf("rules grant %s, which is not terminal control:\n%s", forbidden, rules)
		}
	}
}

// A batch job with no terminal gets no ioctl allowance: it needs none, and
// the smallest policy that works is the right one.
func TestNoTTYMeansNoIoctlAllowance(t *testing.T) {
	prof, err := GenerateProfile(ProfileConfig{ScratchDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(prof, "file-ioctl") {
		t.Error("a job with no terminal was granted terminal control")
	}
}

// A job that turns out to have a terminal gets the allowance appended, since
// its policy was written before the pty was allocated.
func TestLaunchAppendsTTYRules(t *testing.T) {
	dir := t.TempDir()
	// A real profile, not a minimal one: a policy that denies everything
	// cannot exec anything, so loading it would fail for reasons unrelated
	// to what is being tested.
	base, err := GenerateProfile(ProfileConfig{ScratchDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	prof := dir + "/p.sb"
	if err := os.WriteFile(prof, []byte(base), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := appendTTYRules(prof, "/dev/ttys099"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(prof)
	if !strings.Contains(string(b), `(allow file-ioctl (literal "/dev/ttys099"))`) {
		t.Errorf("the terminal allowance was not appended:\n%s", b)
	}
	// Appended, so it comes after the denies it narrows.
	if strings.Index(string(b), "(deny default)") > strings.Index(string(b), "file-ioctl") {
		t.Error("the allowance was written before the deny it must override")
	}
	// And the profile still loads.
	if out, err := exec.Command("sandbox-exec", "-f", prof, "/usr/bin/true").CombinedOutput(); err != nil {
		t.Errorf("the appended profile does not load: %v\n%s", err, out)
	}
}

// The runtime refuses a memory limit under 200 MiB and defaults to one
// gigabyte when given none. Both are traps: the first makes a small job fail
// to start with a message about the runtime, the second silently caps a job
// that asked for no limit at all.
func TestContainerMemoryLimitAvoidsTheRuntimesTraps(t *testing.T) {
	const machine = 16 << 30 // a 16 GiB Mac

	// Under the floor is raised to it, not passed through.
	if got := containerMemMB(job.Spec{Limits: job.Limits{MemBytes: 100 << 20}}, machine); got != MinContainerMemMB {
		t.Errorf("a 100 MiB request produced %d MiB, want the %d MiB floor",
			got, MinContainerMemMB)
	}
	// Above the floor, the hard limit sits above what was asked for, so
	// shome's poller sees the job cross its real limit and can say so --
	// the virtual machine's kill is silent and, for a backgrounded child,
	// invisible to the script's exit status.
	got4 := containerMemMB(job.Spec{Limits: job.Limits{MemBytes: 4 << 30}}, machine)
	if got4 <= 4096 {
		t.Errorf("a 4 GiB request produced a %d MiB hard limit; it must leave "+
			"room above the request for shome to notice first", got4)
	}
	if got4 > 4096+4096/2 {
		t.Errorf("a 4 GiB request produced %d MiB; that is far more headroom "+
			"than the machine can afford to hand out", got4)
	}
	// No limit means the machine, not the runtime's one-gigabyte default.
	got := containerMemMB(job.Spec{}, machine)
	if got <= 1024 {
		t.Errorf("an unlimited job was capped at %d MiB, which is the runtime's "+
			"default rather than the machine's memory", got)
	}
	if got >= machine>>20 {
		t.Errorf("an unlimited job was given %d MiB, leaving nothing for the host", got)
	}
	// A machine whose memory is unknown gets no limit rather than a wrong one.
	if got := containerMemMB(job.Spec{}, 0); got != 0 {
		t.Errorf("with unknown machine memory the limit was %d MiB, want none", got)
	}
}
