package userenv

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The base software environment: what an account gets on every machine,
// beyond a shell.
//
// # Why a fixed set
//
// A job script that works on one node and not another because of a missing
// tool is an unpleasant thing to debug. The set below is therefore the same
// everywhere, guaranteed by shome rather than by whoever set the machine up.
//
// # Two realizations of one list
//
// Where a job runs in a container -- every session, and every job that does
// not ask for a GPU -- the list is installed into an image and is exactly
// this. Where a job must run natively -- a GPU job on a Mac, because Metal
// does not exist inside a virtual machine -- there is no image to install,
// so shome finds the machine's own copies instead and opens the sandbox
// onto them. That is why each entry names its commands as well as its
// package: the container needs the Debian package name, and the native side
// needs to know what to look for on disk.
//
// The two are not guaranteed identical -- a machine that has never had the
// developer tools installed has no git to find -- so the native side reports
// what it actually resolved rather than assuming. See MachineSoftware.
//
// # What accounts should install for themselves
//
// Anything language-level, with uv: `uv tool install ruff` lands in
// ~/.local/bin, which is inside the account's own directory and therefore
// persists, syncs with `shome fs`, and counts against their disk limit. The
// base set is deliberately the things uv cannot provide -- git, a pager, an
// editor, an ssh client.

// Package is one entry in the base software environment.
type Package struct {
	// Name is the Debian package, for the container image.
	Name string
	// Commands are what it puts on PATH. Empty for a package that installs
	// no program of its own, like a certificate bundle.
	Commands []string
}

// BaseSoftware is the software every session and every job gets.
//
// Kept small and justified rather than comprehensive. Each of these is
// either required for something else to work at all (ca-certificates,
// without which every HTTPS fetch fails) or is a tool whose absence makes a
// login node strange to use (an editor, a pager, ps).
//
// Deliberately absent:
//
//   - python and pip, because uv provides interpreters into the account's own
//     directory where they persist; a system one would be a second,
//     unmanaged copy.
//   - a compiler, because it is a quarter of a gigabyte for a case uv's
//     prebuilt wheels usually avoid. An admin who needs one adds it.
//   - tmux, because a container is destroyed when its session ends, so a
//     detached tmux session could not survive a disconnect. Shipping it
//     would imply a persistence that does not exist.
var BaseSoftware = []Package{
	{Name: "ca-certificates"}, // every HTTPS fetch depends on it
	{Name: "curl", Commands: []string{"curl"}},
	{Name: "wget", Commands: []string{"wget"}}, // scripts assume one or the other
	{Name: "file", Commands: []string{"file"}},
	// GitHub: cloning private repos, pull requests, releases.
	{Name: "gh", Commands: []string{"gh"}},
	{Name: "git", Commands: []string{"git"}},
	// Model weights and datasets are usually LFS-backed.
	{Name: "git-lfs", Commands: []string{"git-lfs"}},
	{Name: "jq", Commands: []string{"jq"}},
	// git log and git diff are unusable without a pager.
	{Name: "less", Commands: []string{"less"}},
	{Name: "nano", Commands: []string{"nano"}},
	// vim, not vim-tiny. The tiny package installs one binary called
	// vim.tiny and registers it only as an `editor` alternative, which nano
	// then wins on priority -- so a session built with it had no `vim`
	// command at all, which is not what anyone asking for vim means. It is
	// also compiled without +eval and +syntax, so a .vimrc with a
	// conditional in it errors on startup and nothing is highlighted.
	//
	// The real package costs about 40 MB of image, most of it vim-runtime.
	// Worth it for an editor people actually know how to leave, and it
	// matches the native path, where a Mac's own /usr/bin/vim is the full
	// build.
	{Name: "vim", Commands: []string{"vim"}},
	// git over ssh, scp, ssh-keygen.
	{Name: "openssh-client", Commands: []string{"ssh", "ssh-keygen", "scp"}},
	{Name: "procps", Commands: []string{"ps", "top"}},
	{Name: "rsync", Commands: []string{"rsync"}},
	{Name: "tree", Commands: []string{"tree"}},
	{Name: "unzip", Commands: []string{"unzip"}},
	{Name: "zip", Commands: []string{"zip"}},
}

// BasePackages is the base set as package names, for building the image.
func BasePackages() []string {
	out := make([]string, 0, len(BaseSoftware))
	for _, p := range BaseSoftware {
		out = append(out, p.Name)
	}
	return out
}

// validPackage reports whether a name is a Debian package name.
//
// Checked because these are interpolated into a build file that runs as
// root inside the image: a name carrying a space or a semicolon would be a
// command, not a package.
func validPackage(p string) bool {
	if len(p) < 2 || len(p) > 64 {
		return false
	}
	for i, r := range p {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case (r == '+' || r == '-' || r == '.') && i > 0:
		default:
			return false
		}
	}
	return true
}

// Packages is the full set for an image: the base plus an admin's additions.
//
// Sorted and deduplicated so the same set always produces the same tag, and
// so a rebuild is triggered by a real change rather than by reordering.
func Packages(extra []string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, p := range append(BasePackages(), extra...) {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		if !validPackage(p) {
			return nil, fmt.Errorf("%q is not a valid package name", p)
		}
		seen[p] = true
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}

// Extra packages an admin wants in the session image, on top of shome's own
// set.
//
// A separate list rather than a replaceable one: shome guarantees what every
// session has, so a script that works on one machine works on the next, and
// the admin adds what this cluster happens to need. Replacing the base
// wholesale is how two machines end up quietly different.

// PackagesFile is where the additions live.
func PackagesFile(root string) string {
	return filepath.Join(Dir(root), "extra-packages")
}

const packagesHeader = `# Extra software for logged-in accounts, one package name per line.
#
# These are added to the set shome installs itself -- git, curl, an editor,
# ssh and so on -- which every session has and which this file cannot remove.
# Debian package names, since the session image is Debian.
#
# Takes effect on the next 'shome admin env install', which rebuilds the
# image. Lines starting with # are ignored.
#
# For example:
#   build-essential
#   tmux
`

// ExtraPackages reads the admin's additions.
//
// A missing file is not an error: no additions is the ordinary case.
func ExtraPackages(root string) ([]string, error) {
	b, err := os.ReadFile(PackagesFile(root))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		if p := strings.TrimSpace(line); p != "" {
			out = append(out, p)
		}
	}
	return out, nil
}

// SetExtraPackages writes the additions, sorted and deduplicated.
func SetExtraPackages(root string, pkgs []string) error {
	if err := os.MkdirAll(Dir(root), 0o755); err != nil {
		return err
	}
	seen := map[string]bool{}
	var keep []string
	for _, p := range pkgs {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		keep = append(keep, p)
	}
	sort.Strings(keep)

	var b strings.Builder
	b.WriteString(packagesHeader)
	for _, p := range keep {
		b.WriteString(p + "\n")
	}
	tmp := PackagesFile(root) + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, PackagesFile(root))
}

// AddExtraPackages adds to the list, and reports what was new.
func AddExtraPackages(root string, add []string) ([]string, error) {
	cur, err := ExtraPackages(root)
	if err != nil {
		return nil, err
	}
	have := map[string]bool{}
	for _, p := range cur {
		have[p] = true
	}
	var added []string
	for _, p := range add {
		p = strings.TrimSpace(p)
		if p == "" || have[p] {
			continue
		}
		have[p] = true
		added = append(added, p)
		cur = append(cur, p)
	}
	if len(added) == 0 {
		return nil, nil
	}
	return added, SetExtraPackages(root, cur)
}

// RemoveExtraPackages drops from the list, and reports what went.
//
// Only ever removes an addition. shome's own set is not in this file and
// cannot be taken out of it, which is the point of keeping them separate.
func RemoveExtraPackages(root string, drop []string) ([]string, error) {
	cur, err := ExtraPackages(root)
	if err != nil {
		return nil, err
	}
	gone := map[string]bool{}
	for _, p := range drop {
		gone[strings.TrimSpace(p)] = true
	}
	var keep, removed []string
	for _, p := range cur {
		if gone[p] {
			removed = append(removed, p)
			continue
		}
		keep = append(keep, p)
	}
	if len(removed) == 0 {
		return nil, fmt.Errorf("none of those are in the list; "+
			"see 'shome admin env packages' (%s)", PackagesFile(root))
	}
	return removed, SetExtraPackages(root, keep)
}

// ImageDir is where the session image's build files live.
func ImageDir(root string) string { return filepath.Join(Dir(root), "image") }
