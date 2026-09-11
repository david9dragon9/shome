package userenv

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// The native realization of the base software environment.
//
// # Why there are two
//
// Almost everything runs in a container: every login session, and every job
// that does not ask for a GPU. Those get the image, which holds exactly
// BaseSoftware and nothing of the machine.
//
// A GPU job on a Mac cannot. Metal does not exist inside a virtual machine
// (see docs/design-notes.md), so the job runs natively under a Seatbelt profile, and a profile
// is a policy over the real filesystem rather than a new one -- there is no
// image to install into and no mount to place. The only software such a job
// can run is software the machine already has.
//
// So the native side does the reverse of the container side: instead of
// installing the list, it looks for it, and opens the sandbox onto the trees
// where it found something.
//
// # Why whole trees
//
// Allowing the individual binaries would not work. A command is not a file:
// git needs its helpers in libexec and its templates in share, and a
// Homebrew binary is a symlink into a versioned cellar directory with its
// own dynamic libraries. The unit that is reliably self-contained is the
// installation prefix, so that is what gets allowed -- read-only, and only
// when it actually provides something.
//
// That is a real widening of what a native job can see, and it is confined
// to the native path on purpose: a prefix names the software its owner
// installed. It is not opened speculatively. A machine with no Homebrew tool
// in the base set never has /opt/homebrew allowed, and nothing outside these
// trees is exposed by any of it -- not the owner's home, not /Applications,
// not another account's files.

// SoftwareTree is a directory holding software installed on this machine.
type SoftwareTree struct {
	// Root is the tree a sandbox must allow reading whole.
	Root string
	// Bin are the directories on it that hold commands, in PATH order.
	Bin []string
	// Provides are the base commands found here.
	Provides []string
}

// candidateTrees are the conventional prefixes for installed software.
//
// Only darwin has any. On Linux a job runs inside a mount namespace that
// binds the machine's /usr whole, so its software is already there and
// there is nothing extra to open; on a platform with no native path at all
// the question does not arise.
func candidateTrees() []SoftwareTree {
	if runtime.GOOS != "darwin" {
		return nil
	}
	return []SoftwareTree{
		// The Command Line Tools, which is where git comes from on a Mac.
		//
		// Deliberately this and not the Xcode developer directory that
		// /usr/bin/git forwards to: that path is inside an application
		// bundle, it is gigabytes, and opening it would expose an
		// application rather than a tool prefix. A machine with Xcode but
		// no Command Line Tools reports git as missing, and the fix --
		// xcode-select --install -- is a normal thing to ask of a machine
		// joining a cluster.
		{Root: "/Library/Developer/CommandLineTools", Bin: []string{"usr/bin"}},
		// Homebrew, on Apple silicon and on Intel respectively.
		{Root: "/opt/homebrew", Bin: []string{"bin", "sbin"}},
		{Root: "/usr/local", Bin: []string{"bin", "sbin"}},
		// MacPorts.
		{Root: "/opt/local", Bin: []string{"bin", "sbin"}},
	}
}

// SystemBinDirs are the OS's own command directories.
//
// Always readable inside the sandbox and always on PATH, so a command found
// in one of them needs no tree opened for it.
var SystemBinDirs = []string{"/usr/bin", "/bin", "/usr/sbin", "/sbin"}

// baseCommands is every command the base software environment should offer.
func baseCommands() []string {
	var out []string
	for _, p := range BaseSoftware {
		out = append(out, p.Commands...)
	}
	return out
}

// MachineSoftware reports the trees this machine keeps base software in.
//
// A tree is opened only when it provides a base command, so a machine with
// no Homebrew copy of anything in the set never has /opt/homebrew allowed.
//
// A command already present in a system directory does not settle the
// question, which is not an oversight. On macOS the copy in /usr/bin is
// often a stub: /usr/bin/git is one of seventy-odd hard links to a small
// forwarder that re-execs the real binary out of whichever developer
// directory is selected. Inside the sandbox that target is not allowed, so
// the stub fails -- and it fails after reporting a version, which is a
// thoroughly confusing way to find out. The tree's own copy is therefore
// preferred, and PATH is ordered to match.
func MachineSoftware() []SoftwareTree {
	return machineSoftware(candidateTrees(), baseCommands())
}

// machineSoftware is MachineSoftware over an explicit candidate set, so the
// selection rule can be tested against directories a test builds rather than
// against whatever the machine running the test happens to have installed.
func machineSoftware(candidates []SoftwareTree, commands []string) []SoftwareTree {
	want := map[string]bool{}
	for _, c := range commands {
		want[c] = true
	}
	var trees []SoftwareTree
	for _, t := range candidates {
		if fi, err := os.Stat(t.Root); err != nil || !fi.IsDir() {
			continue
		}
		var bins []string
		for _, b := range t.Bin {
			d := filepath.Join(t.Root, b)
			if fi, err := os.Stat(d); err == nil && fi.IsDir() {
				bins = append(bins, d)
			}
		}
		if len(bins) == 0 {
			continue
		}
		var provides []string
		for _, c := range commands {
			if want[c] && findIn(bins, c) != "" {
				provides = append(provides, c)
			}
		}
		if len(provides) == 0 {
			continue
		}
		// Claimed, so a later tree is not opened for the same command.
		for _, c := range provides {
			delete(want, c)
		}
		trees = append(trees, SoftwareTree{Root: t.Root, Bin: bins, Provides: provides})
	}
	return trees
}

// SoftwareRoots is MachineSoftware as the trees a sandbox must allow.
func SoftwareRoots(trees []SoftwareTree) []string {
	out := make([]string, 0, len(trees))
	for _, t := range trees {
		out = append(out, t.Root)
	}
	return out
}

// SoftwareBinDirs is MachineSoftware as PATH entries, in order.
func SoftwareBinDirs(trees []SoftwareTree) []string {
	var out []string
	for _, t := range trees {
		out = append(out, t.Bin...)
	}
	return out
}

// NativeTool is one base command as resolved on this machine's filesystem.
type NativeTool struct {
	Package string
	Command string
	Path    string // empty when this machine does not have it
}

// NativeStatus reports which of the base commands a natively-run job finds.
//
// Reported rather than assumed, because unlike the image this set is not
// installed by shome: it is whatever the machine happens to have. An admin
// looking at a Mac that will run GPU jobs needs to be able to see the gap
// before a job does.
func NativeStatus() []NativeTool {
	dirs := append(SoftwareBinDirs(MachineSoftware()), SystemBinDirs...)
	var out []NativeTool
	for _, p := range BaseSoftware {
		for _, c := range p.Commands {
			out = append(out, NativeTool{Package: p.Name, Command: c, Path: findIn(dirs, c)})
		}
	}
	return out
}

// findIn returns the first directory entry that is an executable file.
func findIn(dirs []string, name string) string {
	for _, d := range dirs {
		p := filepath.Join(d, name)
		fi, err := os.Stat(p)
		if err != nil || fi.IsDir() || fi.Mode()&0o111 == 0 {
			continue
		}
		if isDeveloperStub(p) {
			continue
		}
		return p
	}
	return ""
}

// stubReference is the file the developer-tool stubs in /usr/bin are hard
// links to.
//
// /usr/bin/clang is always one of them on macOS: the real compiler lives in
// the selected developer directory, and this is the forwarder that finds it.
// Comparing against it identifies the whole family -- git, make, swift and
// the rest -- without a list that would go out of date.
const stubReference = "/usr/bin/clang"

// isDeveloperStub reports whether a path is one of those forwarders.
//
// They are excluded from the search because the sandbox does not allow what
// they forward to. A stub reports a version quite happily and then fails on
// the first real operation, so treating one as a working command would be
// worse than reporting the command missing.
func isDeveloperStub(p string) bool {
	if runtime.GOOS != "darwin" || !strings.HasPrefix(p, "/usr/bin/") {
		return false
	}
	ref, err := os.Stat(stubReference)
	if err != nil {
		return false
	}
	fi, err := os.Stat(p)
	if err != nil {
		return false
	}
	return os.SameFile(fi, ref)
}
