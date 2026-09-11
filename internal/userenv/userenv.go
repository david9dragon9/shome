// Package userenv gives a logged-in account a usable working environment.
//
// # The problem it solves
//
// A remote user could previously run a fixed list of shome verbs and nothing
// else. That is enough to submit a job and read its output, and not enough to
// prepare one: no editor, no interpreter, no way to install a dependency.
//
// # The shape of the answer
//
// Each account gets a directory on each machine -- the same directory
// `shome fs` manages -- which becomes $HOME for an isolated interactive
// session. Inside it they may do as they like. Outside it they can reach
// nothing: not the machine owner's files, not another account's, and not
// shome's own state, which holds the admin token and the certificate
// authority's private key.
//
// uv is the package manager because it needs no system privilege, installs
// its own Python interpreters, and can be pointed entirely inside a single
// directory. That last property is what makes it fit: everything it writes
// lands somewhere shome already accounts for, so an environment a user builds
// is covered by their disk limit and can be copied to another machine with
// `shome fs sync` like any other files.
//
// # Where things go
//
// Every path uv might write to is redirected under $HOME, and none of it is
// left to uv's defaults. A cache in the caller's real home would escape the
// sandbox, escape the quota, and be invisible to `shome fs`.
package userenv

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Dir is where shome keeps the managed environment on a machine.
//
// Beside the accounts rather than inside any one of them: the tools are
// shared, read-only, and identical for everybody, which is what makes the
// environment reproducible across machines.
func Dir(root string) string { return filepath.Join(root, "env") }

// ToolDir holds the managed binaries a session may run.
func ToolDir(root string) string { return filepath.Join(Dir(root), "bin") }

// UVPath is the managed uv binary.
func UVPath(root string) string { return filepath.Join(ToolDir(root), "uv") }

// RCPath is the shell startup file every session sources.
func RCPath(root string) string { return filepath.Join(Dir(root), rcName) }

// Tool is one managed binary and what it reports about itself.
type Tool struct {
	Name    string
	Path    string
	Version string
	Present bool
}

// Status reports what the managed environment currently offers.
func Status(root string) []Tool {
	out := []Tool{}
	for _, name := range []string{"uv", "uvx"} {
		p := filepath.Join(ToolDir(root), name)
		t := Tool{Name: name, Path: p}
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			t.Present = true
			t.Version = version(p)
		}
		out = append(out, t)
	}
	return out
}

// Ready reports whether a session will have a package manager.
func Ready(root string) bool {
	for _, t := range Status(root) {
		if t.Name == "uv" {
			return t.Present
		}
	}
	return false
}

func version(bin string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--version").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// Install populates the managed tool directory.
//
// It copies uv from wherever this machine already has it rather than
// downloading anything. That is a deliberate limit: fetching and executing a
// binary from the network on the machine somebody has lent you is not a thing
// shome should do on its own initiative, and an admin who wants a specific
// version has a package manager of their own for that.
//
// Copied rather than symlinked because the usual place uv lives is inside the
// owner's home directory, which every session's sandbox denies -- a symlink
// there would resolve to a path the session cannot read, and fail as though
// uv were broken.
func Install(root string) ([]string, error) {
	if err := os.MkdirAll(ToolDir(root), 0o755); err != nil {
		return nil, err
	}
	var did []string
	found := false
	for _, name := range []string{"uv", "uvx"} {
		src, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		found = true
		dst := filepath.Join(ToolDir(root), name)
		if err := copyExecutable(src, dst); err != nil {
			return did, fmt.Errorf("copy %s: %w", name, err)
		}
		did = append(did, fmt.Sprintf("%s %s (from %s)", name, version(dst), src))
	}
	if !found {
		return nil, fmt.Errorf("uv is not installed on this machine, so there is "+
			"nothing to copy.\n\nInstall it first, then run this again:\n"+
			"  %s\n\nshome deliberately does not download it for you: fetching and "+
			"running\na binary from the network on a machine someone has lent you is "+
			"not a\ndecision shome should make by itself.", installHint())
	}
	if err := writeRC(root); err != nil {
		return did, err
	}
	did = append(did, "session startup file "+RCPath(root))
	return did, nil
}

func installHint() string {
	if runtime.GOOS == "darwin" {
		return "brew install uv    (or: curl -LsSf https://astral.sh/uv/install.sh | sh)"
	}
	return "curl -LsSf https://astral.sh/uv/install.sh | sh"
}

// copyExecutable copies src to dst atomically and makes it executable.
func copyExecutable(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	// Replaced by rename, so a session starting mid-upgrade sees either the
	// old binary or the new one and never a half-written file.
	return os.Rename(tmp, dst)
}

// Session is where an account's things are, as the session or job running
// inside the isolation will see them.
//
// The paths are the ones that will exist inside, which need not be the ones
// on the host: a mount namespace or a container can put a directory
// anywhere, so on Linux and inside a Mac container an account's home appears
// at a path that says nothing about where it lives. HOME naming a directory
// the process cannot see is a session that cannot start.
type Session struct {
	// Home is $HOME as seen from inside.
	Home string
	// Tools is shome's managed tool directory as seen from inside. Empty
	// when it is not there, which is the case inside a container that was
	// not given one.
	Tools string
	// Software are the machine's own software prefixes, as PATH entries.
	//
	// Set only where the process runs on the host's own filesystem -- a GPU
	// job on a Mac -- because that is the one case with no image to provide
	// the base software. See MachineSoftware.
	Software []string

	// Slot is the environment's os-arch, as platform.Layout reports it.
	//
	// One home directory is shared by every environment an account can run
	// in, and on a Mac there are two of them: a Linux container for most
	// work, the host itself for a GPU job. A virtualenv, an installed tool
	// and a downloaded interpreter are all compiled code, so each
	// environment gets its own directory for them -- see SlotDir. Data,
	// which is the point of the home directory, stays shared.
	//
	// Empty means this process's own, which is right for a caller that is
	// not crossing an isolation boundary.
	Slot string

	User, Cluster, Host string
}

// LocalSlot is the running process's own os-arch.
func LocalSlot() string { return runtime.GOOS + "-" + runtime.GOARCH }

// slot is the session's slot, defaulting to this process's.
func (s Session) slot() string {
	if s.Slot != "" {
		return s.Slot
	}
	return LocalSlot()
}

// SlotDir is where an account's compiled software lives for one environment.
//
// Inside the home directory, so it syncs with `shome fs`, counts against the
// account's disk limit, and is removed with the account. Named by os-arch,
// so the same home can hold a Linux virtualenv for container jobs and a
// macOS one for GPU jobs without either finding the other's binaries -- the
// failure that produced was `python: Operation not permitted`, from a
// perfectly good Linux interpreter that a macOS job had found first on PATH.
func SlotDir(home, slot string) string {
	if slot == "" {
		slot = LocalSlot()
	}
	return filepath.Join(home, ".shome", slot)
}

// VenvDir is the virtualenv activated automatically for a slot.
func VenvDir(home, slot string) string { return filepath.Join(SlotDir(home, slot), "venv") }

// SlotBinDir is where a slot's installed commands go, first on PATH.
func SlotBinDir(home, slot string) string { return filepath.Join(SlotDir(home, slot), "bin") }

// SlotPythonDir is where uv puts interpreters it downloads for a slot.
func SlotPythonDir(home, slot string) string { return filepath.Join(SlotDir(home, slot), "python") }

// Env is the environment the session or job gets, over the platform's base.
//
// Every uv path is set explicitly. Left to its defaults uv would cache under
// the *calling process's* home -- the machine owner's -- which would escape
// the sandbox, escape the account's disk limit, and leave files `shome fs`
// cannot see or clean up.
func (s Session) Env() map[string]string {
	share := filepath.Join(s.Home, ".local", "share")
	return map[string]string{
		"PATH": s.Path(),

		// XDG first, so anything else respecting it also stays inside.
		"XDG_CACHE_HOME":  filepath.Join(s.Home, ".cache"),
		"XDG_DATA_HOME":   share,
		"XDG_CONFIG_HOME": filepath.Join(s.Home, ".config"),
		"XDG_STATE_HOME":  filepath.Join(s.Home, ".local", "state"),

		// Then uv explicitly, because its defaults do not all follow XDG.
		"UV_CACHE_DIR": filepath.Join(s.Home, ".cache", "uv"),
		// Per slot, all three: an installed tool, a downloaded interpreter
		// and a virtualenv are compiled for one os-arch and unusable from
		// the other. The cache is deliberately shared -- uv keys it by
		// wheel, so the same download serves both -- and it is the part
		// large enough to be worth sharing.
		"UV_TOOL_DIR":           filepath.Join(s.slotDir(), "tools"),
		"UV_TOOL_BIN_DIR":       SlotBinDir(s.Home, s.slot()),
		"UV_PYTHON_INSTALL_DIR": SlotPythonDir(s.Home, s.slot()),
		// So `uv sync` and `uv run` use the environment the session
		// activates rather than making a second one in the current
		// directory.
		"UV_PROJECT_ENVIRONMENT": s.Venv(),
		// Named for scripts and for the startup file: `uv venv $SHOME_VENV`
		// is the one command that creates the environment a job will find.
		"SHOME_VENV": s.Venv(),
		"SHOME_SLOT": s.slot(),
		// only-managed, not managed: uv should install its own interpreter
		// into the account's directory, never fall back to one belonging to
		// the machine.
		//
		// "managed" is a preference, not a rule -- when uv cannot download
		// (a job with no network) it quietly uses whatever it can find, and
		// on a Mac that is now CPython 3.9 from the Command Line Tools,
		// because the native path puts those on PATH. A user asking for
		// torch got a 3.9 virtualenv and an unexplainable resolution
		// failure. There is no system Python inside the container either,
		// so refusing here also makes the two paths agree.
		"UV_PYTHON_PREFERENCE": "only-managed",

		// The machine's system-wide git configuration does not apply to
		// cluster work. Inside the image there is no /etc/gitconfig to read;
		// natively there is one, it belongs to the machine's owner, and the
		// sandbox refuses it -- which git treats as fatal rather than as
		// absent, so every command fails with a permissions error. Skipping
		// it deliberately makes the two the same and leaves an account's own
		// ~/.gitconfig, which is theirs, in charge.
		"GIT_CONFIG_NOSYSTEM": "1",

		// macOS prints a notice about zsh from /etc/bashrc on every
		// interactive bash. Correct advice for someone's own laptop, noise
		// in a cluster session where the shell is not theirs to change.
		"BASH_SILENCE_DEPRECATION_WARNING": "1",

		// What the prompt is built from. Exported so a user can see where
		// they are from inside a script, not just in the prompt.
		"SHOME_CLUSTER": s.Cluster,
		"SHOME_HOST":    s.Host,
		"SHOME_USER":    s.User,
	}
}

// Path is the session's PATH: the account's own installs, then shome's
// managed commands, then the machine's software, then the system's.
//
// The account first, so `uv tool install ruff` then `ruff` works without
// explanation. The machine's software before the system directories,
// because on a Mac /usr/bin/git is a stub that forwards to whichever
// developer directory is selected -- reaching the real one directly is both
// faster and the only version the sandbox allows.
func (s Session) Path() string {
	dirs := []string{
		SlotBinDir(s.Home, s.slot()),
		filepath.Join(s.Venv(), "bin"),
		// The account's own scripts. After the slot directories, because a
		// shell script here works in any environment while a binary here
		// works in only one -- and if both provide a command, the one built
		// for this environment is the one that runs.
		filepath.Join(s.Home, ".local", "bin"),
	}
	if s.Tools != "" {
		dirs = append(dirs, s.Tools)
	}
	dirs = append(dirs, s.Software...)
	dirs = append(dirs, "/usr/local/bin")
	dirs = append(dirs, SystemBinDirs...)

	seen := map[string]bool{}
	var out []string
	for _, d := range dirs {
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	return strings.Join(out, ":")
}

// Venv is the virtualenv this session activates.
func (s Session) Venv() string { return VenvDir(s.Home, s.slot()) }

// slotDir is this session's per-environment directory.
func (s Session) slotDir() string { return SlotDir(s.Home, s.slot()) }

// EnsureHome creates the directories a session expects to exist.
//
// Created up front rather than on demand because a shell that starts with no
// writable cache directory fails in ways that read as a broken machine
// ("cannot create temp file") rather than as a missing directory.
func EnsureHome(home string) error { return EnsureHomeSlot(home, "") }

// EnsureHomeSlot is EnsureHome for one environment: it also creates the
// per-slot directories, so uv has somewhere to put a tool the first time it
// installs one rather than failing on a missing parent.
func EnsureHomeSlot(home, slot string) error {
	if err := os.MkdirAll(SlotBinDir(home, slot), 0o700); err != nil {
		return err
	}
	for _, d := range []string{
		home,
		filepath.Join(home, ".cache"),
		filepath.Join(home, ".config"),
		filepath.Join(home, ".local", "bin"),
		filepath.Join(home, ".local", "share"),
		filepath.Join(home, ".local", "state"),
		filepath.Join(home, ".tmp"),
	} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	return nil
}

// ReadOnlyPaths are the trees a session may read inside the installation
// root, which is otherwise denied whole.
func ReadOnlyPaths(root string) []string { return []string{Dir(root)} }

// ShomePath is the copy of the shome binary a session runs.
func ShomePath(root string) string { return filepath.Join(ToolDir(root), "shome") }

// EnsureManaged prepares everything a session needs that does not depend on
// third-party software: the startup file, and a copy of the shome binary.
//
// The binary is copied rather than found on PATH because the usual place it
// lives is the owner's home directory, which every session's sandbox denies.
// A session that could not run `shome squeue` would be a session that cannot
// use the cluster, which is the entire point of having one.
//
// Called on every daemon start, so an upgraded shome upgrades what sessions
// run without a separate step.
func EnsureManaged(root, selfPath string) error {
	if err := os.MkdirAll(ToolDir(root), 0o755); err != nil {
		return err
	}
	if err := EnsureRC(root); err != nil {
		return err
	}
	if selfPath == "" {
		return nil
	}
	dst := ShomePath(root)
	// Skip an identical copy: this runs on every start, and rewriting the
	// binary underneath a live session is worth avoiding.
	if same, err := sameFile(selfPath, dst); err != nil || !same {
		if err := copyExecutable(selfPath, dst); err != nil {
			return err
		}
	}
	// The startup file goes in the tool directory as well as beside it,
	// because that directory is what a mapped session gets mounted and the
	// copy outside it is unreachable from in there.
	if err := os.WriteFile(filepath.Join(ToolDir(root), rcName), []byte(rcTemplate), 0o644); err != nil {
		return err
	}
	return ensureShims(root)
}

// ShimNames are the Slurm-compatible commands shome answers to.
//
// The one list. It is used three times -- for the symlinks `shome shims
// install` creates on a workstation, for the copies a sandboxed login
// session gets, and for the names `shome` recognises when invoked under one
// of them -- and it was briefly three lists, which is how `sshare` ended up
// working on a workstation and not in a session.
//
// A session that has to type `shome sbatch` instead of `sbatch` is a session
// where every existing script and every remembered habit stops working,
// which defeats the point of being Slurm-compatible at all.
var ShimNames = []string{
	"sbatch", "srun", "squeue", "scancel", "sinfo", "sacct", "scontrol", "sshare",
	// squota is shome's own, not a Slurm command -- Slurm spreads the same
	// question across sshare and sacctmgr. It follows the same naming so it
	// is where somebody would look for it, and so it works as a bare command
	// in a session like the rest.
	"squota",
}

// ensureShims links each Slurm command name to the shome binary.
//
// Symlinks rather than copies: shome dispatches on the name it was invoked
// as, and seven more copies of the binary would be tens of megabytes of
// duplication inside the machine's storage for no benefit. They live in the
// same directory as their target, so a session that can read one can read
// all of them.
func ensureShims(root string) error {
	for _, name := range ShimNames {
		link := filepath.Join(ToolDir(root), name)
		if target, err := os.Readlink(link); err == nil && target == "shome" {
			continue
		}
		// Removed first: a symlink cannot be replaced in place, and a stale
		// one pointing somewhere else is worse than none.
		os.Remove(link)
		if err := os.Symlink("shome", link); err != nil {
			return fmt.Errorf("link %s: %w", name, err)
		}
	}
	return nil
}

// sameFile reports whether two paths have identical size and contents.
//
// Size first, so the common case of an unchanged binary costs one stat each
// rather than reading tens of megabytes on every daemon start.
func sameFile(a, b string) (bool, error) {
	fa, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	fb, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	if fa.Size() != fb.Size() {
		return false, nil
	}
	ha, err := hashFile(a)
	if err != nil {
		return false, err
	}
	hb, err := hashFile(b)
	if err != nil {
		return false, err
	}
	return ha == hb, nil
}

func hashFile(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// LinuxToolDir holds the Linux build of shome, for a session that runs inside
// a container on a Mac.
//
// The managed tool directory next to it holds macOS binaries, which are
// useless in a Linux container -- mounting those would give a session a
// `shome` that cannot execute. This is the same set built for the
// architecture the container runs.
func LinuxToolDir(root, arch string) string {
	return filepath.Join(Dir(root), "linux-"+arch, "bin")
}

// HostTools is the directory to expose as the tool directory for an
// isolated environment on this machine.
//
// Two answers, and which one is right depends on what the environment runs
// rather than on what the machine is:
//
//   - An environment that maps paths but shares this kernel -- a mount
//     namespace on Linux -- runs the machine's own binaries, so the managed
//     tool directory is exactly right.
//   - An environment that is a different operating system -- a Linux
//     container on a Mac -- cannot execute Mach-O, so it gets the
//     cross-built Linux set instead. Mounting the managed directory there
//     hands it a `shome` that cannot run.
//
// warn is non-empty when the answer is usable but stale. An error means
// there is nothing to mount, which is not fatal to the caller: an
// environment without the cluster commands still works, it just cannot run
// them.
func HostTools(root string, mapped bool) (dir, warn string, err error) {
	if !mapped || runtime.GOOS != "darwin" {
		return ToolDir(root), "", nil
	}
	return EnsureLinuxTools(root, containerArch(), DistDirs(root))
}

// containerArch is the architecture a Linux container runs on this machine,
// which is the host's: the runtime does not emulate.
func containerArch() string {
	if runtime.GOARCH == "amd64" {
		return "amd64"
	}
	return "arm64"
}

// EnsureLinuxTools populates the Linux tool directory from a cross-built
// binary.
//
// The binary comes from the same place a joining Linux machine gets one --
// the dist directory the install script cross-builds -- so there is one
// answer to "where does the Linux shome come from" rather than two.
func EnsureLinuxTools(root, arch string, distDirs []string) (dir, warn string, err error) {
	var src string
	for _, d := range distDirs {
		p := filepath.Join(d, "linux-"+arch, "shome")
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			src = p
			break
		}
	}
	if src == "" {
		return "", "", fmt.Errorf("no Linux build of shome for %s.\n"+
			"It is cross-built by install.sh into ~/.shome/dist/linux-%s/shome; "+
			"re-run that script on this machine", arch, arch)
	}
	// A cross-build older than the running binary means sessions get an
	// out-of-date command set -- commands added since simply will not exist
	// in there, and the error a user sees is the general help rather than
	// anything about a version. Worth saying out loud.
	warn = staleWarning(src, arch)

	dir = LinuxToolDir(root, arch)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", warn, err
	}
	dst := filepath.Join(dir, "shome")
	if same, err := sameFile(src, dst); err != nil || !same {
		if err := copyExecutable(src, dst); err != nil {
			return "", warn, fmt.Errorf("copy the Linux shome: %w", err)
		}
	}
	// The shell startup file travels with the binaries, because a session
	// inside a container cannot read the host's copy: --rcfile naming a host
	// path leaves bash falling back to the image's default, which is where
	// the "root@<container id>" prompt came from.
	if err := os.WriteFile(filepath.Join(dir, rcName), []byte(rcTemplate), 0o644); err != nil {
		return "", warn, fmt.Errorf("write the session startup file: %w", err)
	}
	// The Slurm names, so a session inside a container has the same commands
	// as one outside it.
	for _, name := range ShimNames {
		link := filepath.Join(dir, name)
		if t, err := os.Readlink(link); err == nil && t == "shome" {
			continue
		}
		os.Remove(link)
		if err := os.Symlink("shome", link); err != nil {
			return "", warn, fmt.Errorf("link %s: %w", name, err)
		}
	}
	return dir, warn, nil
}

// staleWarning reports when the cross-built Linux binary predates the running
// one.
//
// Modification time rather than a version string, because there is no build
// stamp to compare -- but it catches the case that matters: a dist directory
// left behind by an older install, giving every session a command set from
// weeks ago while the host has today's.
func staleWarning(src, arch string) string {
	self, err := os.Executable()
	if err != nil {
		return ""
	}
	a, err := os.Stat(src)
	if err != nil {
		return ""
	}
	b, err := os.Stat(self)
	if err != nil {
		return ""
	}
	if !a.ModTime().Before(b.ModTime().Add(-time.Minute)) {
		return ""
	}
	return fmt.Sprintf("the Linux build of shome is older than this one "+
		"(%s vs %s), so sessions will have an out-of-date command set. "+
		"Refresh it with:  GOOS=linux GOARCH=%s go build -o %s ./cmd/shome",
		a.ModTime().Format("2006-01-02"), b.ModTime().Format("2006-01-02"), arch, src)
}

// DistDirs are the places a cross-built binary for another platform may be.
//
// The installation's own directory first, then the one install.sh writes to.
// Same order the bootstrap server uses to serve a joining machine, so there
// is one answer to "where do other platforms' binaries live".
func DistDirs(root string) []string {
	dirs := []string{filepath.Join(root, "dist")}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".shome", "dist"))
	}
	return dirs
}
