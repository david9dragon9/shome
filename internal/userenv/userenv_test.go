package userenv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The property the whole design rests on: nothing uv writes may land outside
// the account's own directory. A cache in the caller's real home would escape
// the sandbox, escape the disk limit, and be invisible to `shome fs`.
func TestEverySessionPathStaysInsideTheHome(t *testing.T) {
	root := "/opt/shome"
	home := "/opt/shome/users/alice"
	env := Session{Home: home, Tools: ToolDir(root),
		User: "alice", Cluster: "home", Host: "login"}.Env()

	// Every variable that names a place something gets written.
	for _, k := range []string{
		"XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME",
		"UV_CACHE_DIR", "UV_PYTHON_INSTALL_DIR", "UV_TOOL_DIR", "UV_TOOL_BIN_DIR",
	} {
		v, ok := env[k]
		if !ok {
			t.Errorf("%s is not set, so uv would use its own default outside the home", k)
			continue
		}
		if !strings.HasPrefix(v, home+"/") {
			t.Errorf("%s = %q, which is outside %s", k, v, home)
		}
	}
}

// The managed tools must be reachable, and the account's own installs must
// come first so `uv tool install ruff` then `ruff` works.
func TestPATHOrder(t *testing.T) {
	root := "/opt/shome"
	home := "/opt/shome/users/alice"
	parts := strings.Split(Session{Home: home, Tools: ToolDir(root),
		User: "alice", Cluster: "c", Host: "h"}.Path(), ":")

	idx := func(want string) int {
		for i, p := range parts {
			if p == want {
				return i
			}
		}
		return -1
	}
	own := idx(filepath.Join(home, ".local", "bin"))
	tools := idx(ToolDir(root))
	system := idx("/usr/bin")
	if own < 0 || tools < 0 || system < 0 {
		t.Fatalf("PATH is missing an entry: %v", parts)
	}
	if !(own < tools && tools < system) {
		t.Errorf("PATH order is %v; want the account's own bin, then shome's tools, then the system", parts)
	}
}

func TestSessionEnvNamesWhereYouAre(t *testing.T) {
	env := Session{Home: "/r/users/alice", Tools: ToolDir("/r"),
		User: "alice", Cluster: "home", Host: "login"}.Env()
	for k, want := range map[string]string{
		"SHOME_USER": "alice", "SHOME_CLUSTER": "home", "SHOME_HOST": "login",
	} {
		if env[k] != want {
			t.Errorf("%s = %q, want %q -- the prompt is built from these", k, env[k], want)
		}
	}
}

func TestEnsureHomeCreatesWhatAShellNeeds(t *testing.T) {
	home := filepath.Join(t.TempDir(), "alice")
	if err := EnsureHome(home); err != nil {
		t.Fatal(err)
	}
	// A shell with no writable temp or cache directory fails in ways that
	// read as a broken machine, so these exist before the session starts.
	for _, d := range []string{".cache", ".config", ".local/bin", ".local/share", ".tmp"} {
		if fi, err := os.Stat(filepath.Join(home, d)); err != nil || !fi.IsDir() {
			t.Errorf("%s was not created: %v", d, err)
		}
	}
	// Idempotent: a second login must not fail.
	if err := EnsureHome(home); err != nil {
		t.Errorf("second call: %v", err)
	}
}

func TestStatusReportsAbsenceHonestly(t *testing.T) {
	root := t.TempDir()
	if Ready(root) {
		t.Error("Ready() is true with no tools installed")
	}
	for _, tool := range Status(root) {
		if tool.Present {
			t.Errorf("%s reported present in an empty directory", tool.Name)
		}
	}
}

func TestInstallCopiesAndReportsWhatItDid(t *testing.T) {
	root := t.TempDir()
	// A stand-in for uv: Install copies whatever LookPath finds, so a fake
	// on PATH exercises the real code path without needing uv present.
	bin := t.TempDir()
	fake := filepath.Join(bin, "uv")
	script := "#!/bin/sh\necho 'uv 9.9.9 (fake)'\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)

	did, err := Install(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(did) == 0 {
		t.Fatal("Install reported nothing")
	}
	if !Ready(root) {
		t.Error("Ready() is false after a successful install")
	}
	got := Status(root)
	if got[0].Name != "uv" || !got[0].Present {
		t.Fatalf("uv not present: %+v", got)
	}
	if !strings.Contains(got[0].Version, "9.9.9") {
		t.Errorf("version = %q, want the binary's own report", got[0].Version)
	}
	// A copy, not a symlink: uv usually lives in the owner's home, which
	// every session's sandbox denies, so a link there would be unreadable.
	fi, err := os.Lstat(UVPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Error("uv was symlinked; it must be copied out of the owner's home")
	}
	if fi.Mode()&0o111 == 0 {
		t.Error("the copied uv is not executable")
	}
	// Re-installing over an existing copy must work, for upgrades.
	if _, err := Install(root); err != nil {
		t.Errorf("second install: %v", err)
	}
}

func TestInstallSaysWhatToDoWhenUVIsAbsent(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := Install(t.TempDir())
	if err == nil {
		t.Fatal("Install succeeded with no uv on PATH")
	}
	// The message has to carry the fix, since this is the first thing an
	// admin hits on a fresh machine.
	if !strings.Contains(err.Error(), "astral.sh") {
		t.Errorf("error does not say how to install uv: %v", err)
	}
}

func TestRCIsWrittenAndRefreshedOnChange(t *testing.T) {
	root := t.TempDir()
	if err := EnsureRC(root); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(RCPath(root))
	if err != nil {
		t.Fatal(err)
	}
	// The prompt the user asked for, and the help that makes the session
	// discoverable.
	for _, want := range []string{"SHOME_USER", "SHOME_HOST", "SHOME_CLUSTER", "shome-help"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("startup file does not mention %s", want)
		}
	}
	// An upgrade that changes the template must take effect.
	os.WriteFile(RCPath(root), []byte("stale\n"), 0o644)
	if err := EnsureRC(root); err != nil {
		t.Fatal(err)
	}
	b2, _ := os.ReadFile(RCPath(root))
	if string(b2) == "stale\n" {
		t.Error("a stale startup file was not replaced")
	}
}

// The account's own settings must still be read, or shome has taken their
// shell away from them.
func TestRCSourcesTheAccountsOwnBashrcLast(t *testing.T) {
	root := t.TempDir()
	EnsureRC(root)
	b, _ := os.ReadFile(RCPath(root))
	s := string(b)
	i := strings.Index(s, `"$HOME/.bashrc"`)
	if i < 0 {
		t.Fatal("the account's own ~/.bashrc is never sourced")
	}
	// Last, so shome's defaults can be overridden rather than overriding.
	if strings.TrimSpace(s[i:]) != `"$HOME/.bashrc"` && !strings.HasSuffix(strings.TrimSpace(s), `. "$HOME/.bashrc"`) {
		t.Error("~/.bashrc is not sourced last, so a user cannot override shome's defaults")
	}
}

func TestShellArgvUsesTheManagedStartupFile(t *testing.T) {
	root := "/opt/shome"
	argv := ShellArgv(root)
	if len(argv) == 0 {
		t.Fatal("no shell")
	}
	if strings.HasSuffix(argv[0], "bash") {
		joined := strings.Join(argv, " ")
		if !strings.Contains(joined, RCPath(root)) {
			t.Errorf("bash is started without shome's startup file: %v", argv)
		}
	}
}

// A session inside a Linux container needs Linux binaries. Mounting the
// macOS tool directory would give it a `shome` it cannot execute.
func TestEnsureLinuxToolsBuildsFromDist(t *testing.T) {
	root := t.TempDir()
	dist := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dist, "linux-arm64"), 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dist, "linux-arm64", "shome")
	if err := os.WriteFile(bin, []byte("ELF-ish"), 0o755); err != nil {
		t.Fatal(err)
	}

	dir, _, err := EnsureLinuxTools(root, "arm64", []string{"/nonexistent", dist})
	if err != nil {
		t.Fatal(err)
	}
	if dir != LinuxToolDir(root, "arm64") {
		t.Errorf("dir = %q", dir)
	}
	b, err := os.ReadFile(filepath.Join(dir, "shome"))
	if err != nil || string(b) != "ELF-ish" {
		t.Fatalf("the Linux binary was not copied: %q %v", b, err)
	}
	// The Slurm names come too, or a container session loses squeue.
	for _, n := range ShimNames {
		if target, err := os.Readlink(filepath.Join(dir, n)); err != nil || target != "shome" {
			t.Errorf("%s is not linked to shome: %q %v", n, target, err)
		}
	}
	// Idempotent: it runs on every start.
	if _, _, err := EnsureLinuxTools(root, "arm64", []string{dist}); err != nil {
		t.Errorf("second call: %v", err)
	}
}

// Without a cross-built binary, say where it comes from rather than failing
// silently -- this is the first thing an admin hits.
func TestEnsureLinuxToolsExplainsWhatIsMissing(t *testing.T) {
	_, _, err := EnsureLinuxTools(t.TempDir(), "arm64", []string{t.TempDir()})
	if err == nil {
		t.Fatal("no error with no dist directory")
	}
	if !strings.Contains(err.Error(), "install.sh") {
		t.Errorf("error does not say where the binary comes from: %v", err)
	}
}

// A mapped session -- a container on a Mac, a mount namespace on Linux --
// gets the tool directory mounted and nothing else of the host. The shell
// startup file has to be inside it, or bash falls back to the image's
// defaults and the prompt reads "root@<container id>".
func TestStartupFileTravelsWithTheTools(t *testing.T) {
	root := t.TempDir()
	self := filepath.Join(t.TempDir(), "shome")
	if err := os.WriteFile(self, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := EnsureManaged(root, self); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(ToolDir(root), rcName)
	if _, err := os.Stat(inside); err != nil {
		t.Fatalf("no startup file in the tool directory: %v", err)
	}
	// And the argv names it by its path inside, not the host's.
	argv := ShellArgvAt("/opt/shome/bin")
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "/opt/shome/bin/"+rcName) {
		t.Errorf("argv does not name the startup file inside: %v", argv)
	}
	if strings.Contains(joined, root) {
		t.Errorf("argv names a host path a mapped session cannot see: %v", argv)
	}
}

// A cross-built Linux binary older than the running one gives every session
// a command set from whenever it was built. That happened: sessions had a
// shome from a week earlier, so squota and sshare simply did not exist in
// there and the user got the general help instead.
func TestStaleLinuxBuildIsReported(t *testing.T) {
	root := t.TempDir()
	dist := t.TempDir()
	src := filepath.Join(dist, "linux-arm64", "shome")
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Older than the test binary, which stands in for the running shome.
	old := time.Now().Add(-14 * 24 * time.Hour)
	if err := os.Chtimes(src, old, old); err != nil {
		t.Fatal(err)
	}
	_, warn, err := EnsureLinuxTools(root, "arm64", []string{dist})
	if err != nil {
		t.Fatal(err)
	}
	if warn == "" {
		t.Fatal("a two-week-old cross-build was reported as current")
	}
	// The warning has to carry the fix, since the symptom -- a missing
	// command -- says nothing about a stale binary.
	if !strings.Contains(warn, "GOOS=linux") {
		t.Errorf("the warning does not say how to refresh it: %q", warn)
	}

	// A current one is not flagged.
	now := time.Now()
	os.Chtimes(src, now, now)
	if _, warn, err := EnsureLinuxTools(root, "arm64", []string{dist}); err != nil || warn != "" {
		t.Errorf("a current build was flagged as stale: %q (%v)", warn, err)
	}
}

// One home directory, one software environment per kind of machine.
//
// The same account runs in a Linux container for most work and on macOS
// itself for a GPU job, out of the same home directory. A Linux interpreter
// cannot run on macOS, so a shared ~/.venv meant a GPU job found the
// container's Python first on PATH and died with "Operation not permitted"
// -- a perfectly good binary for the wrong operating system.
func TestEachKindOfMachineGetsItsOwnEnvironment(t *testing.T) {
	const home = "/home/alice"
	linux := Session{Home: home, Slot: "linux-arm64", User: "alice"}
	mac := Session{Home: home, Slot: "darwin-arm64", User: "alice"}

	if linux.Venv() == mac.Venv() {
		t.Fatalf("both kinds share one virtualenv at %s", linux.Venv())
	}
	if got := linux.Venv(); got != home+"/.shome/linux-arm64/venv" {
		t.Errorf("venv = %q", got)
	}
	// Inside the home directory, so it syncs, counts against the disk limit,
	// and goes when the account does.
	for _, s := range []Session{linux, mac} {
		for _, p := range []string{s.Venv(), SlotBinDir(s.Home, s.Slot), SlotPythonDir(s.Home, s.Slot)} {
			if !strings.HasPrefix(p, home+"/") {
				t.Errorf("%q is outside the account's home", p)
			}
		}
	}

	// Everything that holds compiled code is per kind; the cache, which uv
	// keys by wheel, is shared.
	le, me := linux.Env(), mac.Env()
	for _, k := range []string{"SHOME_VENV", "UV_TOOL_BIN_DIR", "UV_PYTHON_INSTALL_DIR",
		"UV_TOOL_DIR", "UV_PROJECT_ENVIRONMENT"} {
		if le[k] == me[k] {
			t.Errorf("%s is shared between kinds of machine: %q", k, le[k])
		}
	}
	if le["UV_CACHE_DIR"] != me["UV_CACHE_DIR"] {
		t.Error("the uv cache should be shared; it is keyed by wheel")
	}
	if le["SHOME_SLOT"] != "linux-arm64" || me["SHOME_SLOT"] != "darwin-arm64" {
		t.Errorf("SHOME_SLOT = %q and %q", le["SHOME_SLOT"], me["SHOME_SLOT"])
	}

	// PATH puts this kind's own binaries first, and the account's
	// hand-placed scripts after them: a shell script in ~/.local/bin works
	// anywhere, a binary there works in one kind only.
	path := strings.Split(linux.Path(), ":")
	want := []string{
		SlotBinDir(home, "linux-arm64"),
		linux.Venv() + "/bin",
		home + "/.local/bin",
	}
	for i, w := range want {
		if i >= len(path) || path[i] != w {
			t.Fatalf("PATH = %v, want it to start with %v", path, want)
		}
	}

	// No slot named means this process's own, for a caller that is not
	// crossing an isolation boundary.
	if got := (Session{Home: home}).Venv(); got != VenvDir(home, LocalSlot()) {
		t.Errorf("venv with no slot = %q, want this machine's own", got)
	}
}
