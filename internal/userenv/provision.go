package userenv

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// Making a machine ready to run cluster work, by itself.
//
// # The problem this solves
//
// The base software set has two realizations -- an image, and the machine's
// own copies -- and only the first is built for you. A Mac gets the image
// when an admin runs `shome admin env install`. A Linux machine gets
// nothing: its jobs run in a mount namespace over the host's own /usr, so
// the set is whatever somebody installed there, and on a freshly added
// machine that is a Debian base system with no git, no gh, no uv and no
// editor. The machine joins, reports itself UP, accepts work, and every job
// that needs any of it fails.
//
// Worse, nothing says so. The information was available -- `shome admin env
// status` on that machine lists what is missing -- but it has to be asked
// for, on the machine, by somebody who already suspects.
//
// So a node prepares itself when it starts, and says what it did.
//
// # What this will and will not do
//
// It installs the base set with the machine's own package manager, and only
// when it can do that without asking anyone for a password: root, or sudo
// already configured not to prompt. A machine that would need a password is
// left alone and told what to run, because a daemon that blocks on a
// password prompt at startup is a daemon that does not start.
//
// It is deliberately narrow: the fixed base set, nothing an admin added,
// and nothing at all on a machine whose package manager it does not know.
// The set is the one in BaseSoftware, so the container and the host end up
// with the same commands -- which is the promise that makes a cluster of
// mixed machines usable.
//
// This is a real change in posture and worth naming: shome is installing
// software on a machine somebody has lent it. The alternative was a machine
// that silently cannot run the work it accepts. SHOME_NO_PROVISION=1 turns
// it off, and then the report is all that happens.

// provisionTimeout bounds the whole attempt. Long enough for apt to fetch a
// dozen small packages on a slow link, short enough that a machine with a
// wedged package manager still finishes starting up -- it will run jobs
// that do not need the missing software in the meantime.
const provisionTimeout = 10 * time.Minute

// Provision brings this machine's base software up to the cluster's set.
//
// Returns what it did and what is still missing, so the caller can log both.
// Never fatal: a machine with an incomplete environment is worse than one
// with a complete one and better than one that refuses to start.
func Provision(ctx context.Context, root string, log func(msg string, args ...any)) {
	if log == nil {
		log = func(string, ...any) {}
	}
	if os.Getenv("SHOME_NO_PROVISION") == "1" {
		log("not preparing this machine's software: SHOME_NO_PROVISION is set")
		return
	}
	ctx, cancel := context.WithTimeout(ctx, provisionTimeout)
	defer cancel()

	// uv first: it is shome's own tool rather than the machine's, every
	// session's PATH names it, and it is what a user reaches for first.
	if err := ensureUV(root); err != nil {
		log("no package manager for sessions on this machine", "err", err)
	}

	if runtime.GOOS != "linux" {
		// A Mac's jobs get the image, or -- for the GPU ones, which cannot
		// be contained -- the machine's own software found by prefix.
		// Asking LookPath here would answer for the daemon's PATH, which is
		// neither, and report a Mac as missing commands its jobs have.
		// `shome admin env status` answers it properly.
		return
	}
	missing := MissingBaseCommands()
	if len(missing) == 0 {
		return
	}
	pkgs := packagesFor(missing)
	installer, why := systemInstaller()
	if installer == nil {
		log("this machine is missing some of the cluster's base software, and "+
			"shome cannot install it here",
			"missing", strings.Join(missing, ", "), "why", why)
		return
	}
	log("installing the cluster's base software on this machine",
		"packages", strings.Join(pkgs, ", "))
	if err := installer(ctx, pkgs); err != nil {
		log("could not install the base software; jobs needing it will fail",
			"missing", strings.Join(missing, ", "), "err", err)
		return
	}
	if still := MissingBaseCommands(); len(still) > 0 {
		log("some of the base software is still missing after installing",
			"missing", strings.Join(still, ", "))
		return
	}
	log("this machine now has the cluster's base software", "installed", len(pkgs))
}

// MissingBaseCommands are the base set's commands this machine does not
// have on its own PATH.
//
// Asked of the machine rather than of a package database: what matters to a
// job is whether the command runs, and a package that installed something
// under another name has not provided it.
func MissingBaseCommands() []string {
	var missing []string
	for _, p := range BaseSoftware {
		for _, cmd := range p.Commands {
			if _, err := exec.LookPath(cmd); err != nil {
				missing = append(missing, cmd)
			}
		}
	}
	return missing
}

// packagesFor maps missing commands back to the packages that provide them.
func packagesFor(missing []string) []string {
	want := map[string]bool{}
	for _, c := range missing {
		want[c] = true
	}
	var pkgs []string
	seen := map[string]bool{}
	for _, p := range BaseSoftware {
		for _, cmd := range p.Commands {
			if want[cmd] && !seen[p.Name] {
				seen[p.Name] = true
				pkgs = append(pkgs, p.Name)
			}
		}
	}
	return pkgs
}

// systemInstaller is how to install packages here, or why there is no way.
//
// Only apt, and only unattended. Other package managers are not refused out
// of preference -- shome simply has no package names for them, since the
// base set is written as Debian packages for the session image, and
// guessing a Fedora name from a Debian one is how you install the wrong
// thing.
func systemInstaller() (func(context.Context, []string) error, string) {
	if runtime.GOOS != "linux" {
		// macOS jobs get the image, or the machine's own software for the
		// GPU path, which `shome admin env status` reports on. Homebrew is
		// not shome's to drive.
		return nil, "only Linux machines are prepared this way"
	}
	apt, err := exec.LookPath("apt-get")
	if err != nil {
		return nil, "no apt-get on this machine"
	}
	prefix, why := rootPrefix()
	if prefix == nil {
		return nil, why
	}
	return func(ctx context.Context, pkgs []string) error {
		update := append(prefix(apt), "update", "-qq")
		if out, err := exec.CommandContext(ctx, update[0], update[1:]...).CombinedOutput(); err != nil {
			return fmt.Errorf("apt-get update: %w: %s", err, firstLine(out))
		}
		args := append(prefix(apt), "install", "-y", "-qq", "--no-install-recommends")
		args = append(args, pkgs...)
		cmd := exec.CommandContext(ctx, args[0], args[1:]...)
		// Non-interactive, and with no terminal to prompt on: an installer
		// that stops for a question would hang the whole startup.
		cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
		cmd.Stdin = nil
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("apt-get install: %w: %s", err, firstLine(out))
		}
		return nil
	}, ""
}

// rootPrefix is how to run a command as root here, or why it cannot be.
func rootPrefix() (func(string) []string, string) {
	if os.Geteuid() == 0 {
		return func(bin string) []string { return []string{bin} }, ""
	}
	sudo, err := exec.LookPath("sudo")
	if err != nil {
		return nil, "not running as root and there is no sudo"
	}
	// -n: fail rather than prompt. A daemon starting at boot has nobody to
	// answer, and a password prompt written to a log file is a hang.
	if err := exec.Command(sudo, "-n", "true").Run(); err != nil {
		return nil, "sudo here needs a password, which a daemon cannot answer; " +
			"run 'shome admin env install' on this machine yourself"
	}
	return func(bin string) []string { return []string{sudo, "-n", bin} }, ""
}

func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
