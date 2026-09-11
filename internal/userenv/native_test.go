package userenv

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// mktool writes an executable file, so a test can build a software tree
// without depending on what the machine running it has installed.
func mktool(t *testing.T, dir, name string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// A prefix is a read-only window onto software the machine's owner
// installed, so it is opened only when something in the base set is actually
// there. Opening one speculatively would widen a native job's view of the
// machine for nothing.
func TestOnlyTreesThatProvideSomethingAreOpened(t *testing.T) {
	root := t.TempDir()
	useful := filepath.Join(root, "useful")
	mktool(t, filepath.Join(useful, "bin"), "git")
	empty := filepath.Join(root, "empty")
	if err := os.MkdirAll(filepath.Join(empty, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	absent := filepath.Join(root, "not-installed")
	// Something outside the base set does not earn a tree either.
	unrelated := filepath.Join(root, "unrelated")
	mktool(t, filepath.Join(unrelated, "bin"), "ffmpeg")

	got := machineSoftware([]SoftwareTree{
		{Root: useful, Bin: []string{"bin"}},
		{Root: empty, Bin: []string{"bin"}},
		{Root: absent, Bin: []string{"bin"}},
		{Root: unrelated, Bin: []string{"bin"}},
	}, []string{"git", "less"})

	if len(got) != 1 || got[0].Root != useful {
		t.Fatalf("opened %v; want only %s", SoftwareRoots(got), useful)
	}
	if len(got[0].Provides) != 1 || got[0].Provides[0] != "git" {
		t.Errorf("provides %v; want [git]", got[0].Provides)
	}
}

// The first tree that has a command owns it, so a machine with two copies of
// git does not have both prefixes opened.
func TestASecondTreeIsNotOpenedForTheSameCommand(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "first")
	mktool(t, filepath.Join(first, "bin"), "git")
	second := filepath.Join(root, "second")
	mktool(t, filepath.Join(second, "bin"), "git")

	got := machineSoftware([]SoftwareTree{
		{Root: first, Bin: []string{"bin"}},
		{Root: second, Bin: []string{"bin"}},
	}, []string{"git"})
	if len(got) != 1 || got[0].Root != first {
		t.Fatalf("opened %v; want only %s", SoftwareRoots(got), first)
	}
}

// A directory is not a command, and neither is a file nobody can execute.
func TestNonExecutablesDoNotCount(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(filepath.Join(bin, "git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "less"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := machineSoftware([]SoftwareTree{{Root: root, Bin: []string{"bin"}}},
		[]string{"git", "less"}); len(got) != 0 {
		t.Errorf("opened %v for a directory and an unexecutable file", SoftwareRoots(got))
	}
}

// The machine's software has to come before the system directories.
//
// On macOS /usr/bin/git is a stub that re-execs the real binary out of the
// selected developer directory, which the sandbox does not allow. Finding
// the stub first means a job that reports a git version and then fails on
// the first operation.
func TestMachineSoftwareOutranksTheSystemOnPATH(t *testing.T) {
	s := Session{
		Home:     "/home/alice@mini",
		Tools:    "/opt/shome/bin",
		Software: []string{"/Library/Developer/CommandLineTools/usr/bin", "/opt/homebrew/bin"},
	}
	parts := strings.Split(s.Path(), ":")
	idx := func(want string) int {
		for i, p := range parts {
			if p == want {
				return i
			}
		}
		return -1
	}
	own := idx("/home/alice@mini/.local/bin")
	tools := idx("/opt/shome/bin")
	clt := idx("/Library/Developer/CommandLineTools/usr/bin")
	system := idx("/usr/bin")
	if own < 0 || tools < 0 || clt < 0 || system < 0 {
		t.Fatalf("PATH is missing an entry: %v", parts)
	}
	if !(own < tools && tools < clt && clt < system) {
		t.Errorf("PATH is %v; want the account's own, then shome's, then the "+
			"machine's software, then the system", parts)
	}
}

// A session that has no managed tool directory -- inside a container that
// was not given one -- must not get an empty PATH entry, which means the
// working directory and is a way to run the wrong program.
func TestPATHHasNoEmptyOrRepeatedEntries(t *testing.T) {
	s := Session{Home: "/home/alice@mini", Software: []string{"/usr/bin", "/usr/local/bin"}}
	seen := map[string]bool{}
	for _, p := range strings.Split(s.Path(), ":") {
		if p == "" {
			t.Fatalf("PATH has an empty entry: %q", s.Path())
		}
		if seen[p] {
			t.Errorf("PATH repeats %q: %s", p, s.Path())
		}
		seen[p] = true
	}
}

// git reads the machine's system-wide configuration unless told not to. It
// belongs to the machine's owner, the sandbox refuses it, and git treats
// being refused as fatal rather than as absent -- so without this every git
// command in a native job fails with a permissions error.
func TestGitDoesNotReadTheMachinesConfiguration(t *testing.T) {
	env := Session{Home: "/h"}.Env()
	if env["GIT_CONFIG_NOSYSTEM"] != "1" {
		t.Error("GIT_CONFIG_NOSYSTEM is not set")
	}
}

// Every base package must say what it puts on PATH, or the native side has
// nothing to look for and silently reports the machine as complete.
func TestEveryBasePackageNamesItsCommands(t *testing.T) {
	// The one package that installs no program: a certificate bundle.
	noCommands := map[string]bool{"ca-certificates": true}
	for _, p := range BaseSoftware {
		if len(p.Commands) == 0 && !noCommands[p.Name] {
			t.Errorf("%q lists no commands, so a native job cannot be told "+
				"whether this machine has it", p.Name)
		}
		for _, c := range p.Commands {
			if strings.ContainsAny(c, "/ ") {
				t.Errorf("%q is not a bare command name", c)
			}
		}
	}
}

// The candidate prefixes are macOS's, because macOS is the only platform
// with a native path: on Linux a job runs in a mount namespace that binds
// the machine's /usr, so its software is already there.
func TestCandidateTreesAreDarwinOnly(t *testing.T) {
	got := candidateTrees()
	if runtime.GOOS == "darwin" {
		if len(got) == 0 {
			t.Fatal("no candidate software prefixes on a Mac")
		}
		return
	}
	if len(got) != 0 {
		t.Errorf("%s has candidate prefixes %v, but nothing runs natively there",
			runtime.GOOS, SoftwareRoots(got))
	}
}

// uv must install its own interpreter rather than fall back to one belonging
// to the machine. "managed" is only a preference: when uv cannot download --
// a job with no network -- it quietly uses whatever it can find, and on a Mac
// the native path now puts CPython 3.9 from the Command Line Tools on PATH.
// A user asking for torch got a 3.9 virtualenv and an unexplainable
// resolution failure. There is no system Python inside the container either,
// so refusing also makes the two paths agree.
func TestUVWillNotUseTheMachinesPython(t *testing.T) {
	env := Session{Home: "/home/alice@mini"}.Env()
	if got := env["UV_PYTHON_PREFERENCE"]; got != "only-managed" {
		t.Errorf("UV_PYTHON_PREFERENCE = %q, want only-managed", got)
	}
}
