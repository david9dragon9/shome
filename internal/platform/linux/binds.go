package linux

import (
	"os"

	"github.com/davidwu/shome/internal/platform"
)

// The one bind list.
//
// Deliberately free of build tags: the argv is pure data, so the bind set can
// be checked on any machine rather than only on one that has bubblewrap.
//
// The job path and the interactive-session path each had their own copy, and
// two copies of a security boundary is how they drift apart -- the same way
// the shim list and the API handlers did elsewhere in this codebase. Both now
// build from here.
//
// # What a sandboxed process can see
//
// bubblewrap builds a fresh mount namespace containing only what is bound
// into it, so this is an allowlist in the strongest sense: an unbound path is
// not merely forbidden, it does not exist. The set below is the OS's own
// program files plus one writable directory.
//
// Deliberately absent, and therefore invisible: /home, /root, /opt, /srv,
// /mnt, /media, /var, /tmp, and /usr/local. The first two hold the machine
// owner's files; the rest are where locally installed software and other
// people's data live. This matches what the macOS profile allows, so an
// account gets the same view of a Linux node as of a Mac.

// Mount is one host directory placed somewhere inside the sandbox.
//
// In is where it appears, which need not be where it lives. That is the whole
// point of a mount namespace and the reason Linux can give an account a view
// with nothing of the host in it: the path it sees is a name we chose, and
// there is nothing above it to walk up into.
type Mount struct {
	Host string
	// In is the path inside. Empty means the same as Host.
	In string
	// Write makes it read-write. Everything else is read-only.
	Write bool
	// Required fails the sandbox if the host path is missing, rather than
	// silently omitting it. True for the account's own directory.
	Required bool
}

// Where reports where this mount appears inside.
func (m Mount) Where() string {
	if m.In != "" {
		return m.In
	}
	return m.Host
}

// SandboxOpts describes one confined process.
type SandboxOpts struct {
	BwrapPath string
	// Mounts are the directories placed inside, beyond the system ones.
	Mounts []Mount
	// Chdir is the working directory, as seen from inside.
	Chdir string
	// Home is what $HOME becomes inside.
	Home string
	// Network permits outbound network. Off by default, matching macOS.
	Network bool
	// GPU binds the NVIDIA device nodes.
	GPU bool
	// Masks are paths to hide with an empty tmpfs. Only ones that exist:
	// see MaskPaths, and why a missing one is fatal rather than ignored.
	Masks []string
}

// systemBinds are the OS's own program files, read-only.
//
// /usr is bound whole and /usr/local masked over the top, rather than binding
// its subdirectories one by one: the latter breaks in ways that look like
// missing software, because distributions scatter symlinks between them.
var systemBinds = []string{
	"--ro-bind", "/usr", "/usr",
	"--ro-bind-try", "/lib", "/lib",
	"--ro-bind-try", "/lib32", "/lib32",
	"--ro-bind-try", "/lib64", "/lib64",
	"--ro-bind-try", "/bin", "/bin",
	"--ro-bind-try", "/sbin", "/sbin",
	// All of /etc, read-only. Binding only a few files is tempting but breaks
	// in ways that look like missing software: on Debian /usr/bin/awk is a
	// symlink into /etc/alternatives, so without this awk, editors and much
	// else simply "cannot be found" inside the sandbox.
	"--ro-bind-try", "/etc", "/etc",
}

// maskedPaths are hidden even though they sit inside a tree bound above.
//
// An empty tmpfs over each, because bubblewrap has no way to subtract a path
// from a bind. /usr/local is where a machine's owner installs their own
// software, so it names what they have; the /etc entries hold credentials
// that nothing running a batch job has any business reading.
var maskedPaths = []string{
	"/usr/local",
	"/etc/ssh",
	"/etc/NetworkManager",
	"/etc/wpa_supplicant",
	"/etc/sudoers.d",
}

// MaskPaths is the subset of those this machine actually has.
//
// bubblewrap cannot mount a tmpfs over a path that does not exist. The root
// is bound read-only, so it cannot create the mountpoint either, and it
// does not skip the mask -- it fails the whole sandbox:
//
//	bwrap: Can't create file /etc/NetworkManager: Read-only file system
//
// So a machine without one of these could not run a single job. That is not
// a hypothetical Linux: ChromeOS's Crostini container has no NetworkManager,
// which made every job there fail at launch.
//
// Filtering loses nothing. A path that is not there hides nothing, and one
// that appears later is covered the next time a job starts, because this is
// asked per launch rather than once.
//
// exists is passed in rather than called here so the bind set stays a pure
// function that can be tested on any machine -- including one whose /etc
// happens to differ from the machine under test.
func MaskPaths(exists func(string) bool) []string {
	out := make([]string, 0, len(maskedPaths))
	for _, p := range maskedPaths {
		if exists(p) {
			out = append(out, p)
		}
	}
	return out
}

// PathExists is the ordinary answer for MaskPaths.
func PathExists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// SandboxArgv builds the bubblewrap command line that wraps inner.
//
// Pure, so the bind set can be tested anywhere rather than only on a machine
// that has bubblewrap.
func SandboxArgv(o SandboxOpts, inner []string) []string {
	args := []string{
		o.BwrapPath,
		"--unshare-pid", "--unshare-uts", "--unshare-ipc", "--unshare-cgroup-try",
		"--die-with-parent",
		"--proc", "/proc", "--dev", "/dev",
	}
	args = append(args, systemBinds...)
	for _, m := range o.Masks {
		args = append(args, "--tmpfs", m)
	}
	// Last, so nothing above masks them.
	for _, m := range o.Mounts {
		if m.Host == "" {
			continue
		}
		flag := "--ro-bind-try"
		switch {
		case m.Write && m.Required:
			flag = "--bind"
		case m.Write:
			flag = "--bind-try"
		case m.Required:
			flag = "--ro-bind"
		}
		args = append(args, flag, m.Host, m.Where())
	}
	if o.Chdir != "" {
		args = append(args, "--chdir", o.Chdir)
	}
	if o.Home != "" {
		args = append(args, "--setenv", "HOME", o.Home)
	}

	if !o.Network {
		args = append(args, "--unshare-net")
	} else {
		args = append(args, "--share-net")
	}
	if o.GPU {
		// CUDA needs the device nodes and the driver's user-space libraries.
		args = append(args,
			"--dev-bind-try", "/dev/nvidia0", "/dev/nvidia0",
			"--dev-bind-try", "/dev/nvidiactl", "/dev/nvidiactl",
			"--dev-bind-try", "/dev/nvidia-uvm", "/dev/nvidia-uvm")
	}
	return append(args, inner...)
}

// JobScratchPath is where a job's working directory appears inside.
//
// Fixed and uninformative: a job's own scratch is the only place it can
// write, so it needs no name that distinguishes it, and a path derived from
// the installation would publish the host's layout to anything that prints
// its working directory.
const JobScratchPath = "/scratch"

// ToolsPath is where shome's managed tool directory appears inside.
const ToolsPath = "/opt/shome/bin"

// HomePath is where an account's storage appears inside a session.
//
// A bare /home/<user>, with nothing in it naming the machine.
//
// It used to be /home/alice@mini, so that a cluster with no shared
// filesystem did not look like one directory following the user around.
// That name is recorded inside everything the account installs -- a
// virtualenv's interpreter symlinks, a script's shebang, a compiled
// extension's rpath -- so the environment an account built on one machine
// broke on the next, for no reason other than the directory's name. Which
// machine you are on is worth saying, but the prompt, the session banner and
// $SHOME_HOST all say it, and none of them get baked into a file.
func HomePath(user, machine string) string {
	// machine is still in the signature: callers name a placement, and this
	// is the one place that decides whether the name matters.
	_ = machine
	return "/home/" + user
}

// JobArgv is what a job actually runs: its own argument vector, or the
// script that was submitted for it.
//
// The two ways a job arrives are genuinely different, and treating them the
// same is what broke `srun` on a Linux node. `sbatch` sends a script -- its
// contents, so the node can place a copy somewhere the sandbox permits --
// and nothing in Args. `srun bash` and `srun --pty bash` send an argument
// vector and no script at all; Spec.Script holds the command line only so
// that listings have something to show.
//
// Demanding a script for the second case meant the node tried to open a
// file called "bash", which does not exist, and every srun on a Linux node
// failed at launch with:
//
//	launch failed: read job script bash: open bash: no such file or directory
//
// materialise is called only when a script really is wanted, so a caller
// that has an argument vector never pays for writing one out.
//
// The same rule as the macOS backend, deliberately: which of two machines a
// job lands on must not change whether it can start.
func JobArgv(args []string, materialise func() (string, error)) ([]string, error) {
	if len(args) > 0 {
		return args, nil
	}
	path, err := materialise()
	if err != nil {
		return nil, err
	}
	return []string{path}, nil
}

// scriptPathIn is where a job's script appears from inside its sandbox.
//
// Not where it was written. The scratch directory is bound into the
// namespace at a fixed, uninformative path, and the host's own path for it
// is not in there at all -- so a job told to run the host's path got
// "execvp ...: No such file or directory" for a file that had just been
// written successfully.
//
// Here rather than beside the launch code so it can be tested on any
// machine: the mistake is invisible except on Linux, which is where nobody
// was looking.
func scriptPathIn(sb *platform.Sandbox) string {
	dir := sb.ScratchIn
	if dir == "" {
		// No namespace, so no remapping -- and no job either, since a node
		// that cannot confine work drains itself before being offered any.
		dir = sb.ScratchDir
	}
	return dir + "/job-script"
}
