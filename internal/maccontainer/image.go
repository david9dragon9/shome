package maccontainer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// The session image: the software an account finds when it logs in.
//
// # Why an image rather than installing at login
//
// A session's container is destroyed when the session ends, so anything
// installed inside one is gone at logout -- only the account's own directory
// survives. Installing packages per session would therefore mean paying for
// them every time and losing them every time. Building them into an image
// once per machine costs nothing at login and is identical for everybody.
//
// What goes in the image is not decided here: the set lives in
// internal/userenv, because it is the base software environment for the
// whole cluster and a container is only one of the two ways it is realized.
// This file takes a package list and turns it into an image.

// ImageRepo is the name of the image shome builds.
const ImageRepo = "shome-session"

// ImageTag identifies an image by what is in it.
//
// A hash of the base image and the package set, so changing either builds a
// new one and leaving them alone reuses what is there. Naming it by hand --
// "v2" -- is how a machine ends up running an image whose contents nobody
// can name.
func ImageTag(base string, pkgs []string) string {
	h := sha256.Sum256([]byte(base + "\x00" + strings.Join(pkgs, "\x00")))
	return ImageRepo + ":" + hex.EncodeToString(h[:])[:12]
}

// Containerfile is the build recipe.
func Containerfile(base string, pkgs []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# shome session image (generated -- do not edit)\n")
	fmt.Fprintf(&b, "# Rebuilt automatically when the package set changes.\n")
	fmt.Fprintf(&b, "FROM %s\n", base)
	// --no-install-recommends because recommends pull in a surprising amount
	// -- man pages, X libraries -- for a machine nobody looks at directly.
	// The apt lists are removed afterwards: they are tens of megabytes of
	// index that a session never reads.
	fmt.Fprintf(&b, "RUN apt-get update -qq \\\n")
	fmt.Fprintf(&b, " && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends \\\n")
	for i, p := range pkgs {
		sep := " \\"
		if i == len(pkgs)-1 {
			sep = " \\"
		}
		fmt.Fprintf(&b, "      %s%s\n", p, sep)
	}
	fmt.Fprintf(&b, " && rm -rf /var/lib/apt/lists/*\n")
	return b.String()
}

// BuildImage builds the session image if it is not already present.
//
// Returns the tag to run, and whether it had to build. Building needs the
// network and takes a minute or two; running does not, which is why this
// happens at install time rather than at login.
func (r *Runtime) BuildImage(ctx context.Context, dir, base string, pkgs []string) (string, bool, error) {
	tag := ImageTag(base, pkgs)
	if r.HasImage(ctx, tag) {
		return tag, false, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", false, err
	}
	file := filepath.Join(dir, "Containerfile")
	if err := os.WriteFile(file, []byte(Containerfile(base, pkgs)), 0o644); err != nil {
		return "", false, err
	}
	// --arch, because the builder otherwise bootstraps for x86_64 and fails
	// on an Apple silicon Mac with "Rosetta is not installed". The session
	// runs on this machine's own architecture, so that is what to build.
	//
	// "plain", not "none": build takes a different set of progress types
	// from run, and rejects the one run accepts.
	cmd := exec.CommandContext(ctx, r.Bin, "build",
		"--arch", buildArch(),
		"--progress", "plain",
		"--tag", tag, "--file", file, dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", false, buildError(err, out)
	}
	return tag, true, nil
}

// HasImage reports whether an image reference is present locally.
//
// Pulling or building one needs the network and takes a while, so it is an
// admin step rather than something the first person to log in waits for.
func (r *Runtime) HasImage(ctx context.Context, ref string) bool {
	if ref == "" {
		ref = r.Image
	}
	tag := ref
	out, err := exec.CommandContext(ctx, r.Bin, "image", "list", "--quiet").Output()
	if err != nil {
		return false
	}
	name, want, _ := strings.Cut(tag, ":")
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == tag || line == name+":"+want {
			return true
		}
	}
	// Fall back to the full listing, whose format the quiet flag may not
	// match on every version.
	full, err := exec.CommandContext(ctx, r.Bin, "image", "list").Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(full), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == name && f[1] == want {
			return true
		}
	}
	return false
}

// buildArch is the architecture to build for: this machine's own, since that
// is what its sessions run on.
func buildArch() string {
	if runtime.GOARCH == "amd64" {
		return "amd64"
	}
	return "arm64"
}

// buildError turns a build failure into something actionable.
//
// The runtime reports the common one as a VZErrorDomain blob several lines
// long, which says what went wrong deep inside and nothing about what to do.
func buildError(err error, out []byte) error {
	text := string(out)
	if strings.Contains(text, "Rosetta is not installed") {
		return fmt.Errorf("the container runtime cannot build an image on this " +
			"Mac until Rosetta is installed.\n\n" +
			"Its image builder runs as x86_64 even on Apple silicon, so it needs " +
			"Rosetta\nto start -- this is about the builder, not about what shome " +
			"builds.\n\n" +
			"  softwareupdate --install-rosetta\n\n" +
			"Then run 'shome admin env install' again. Sessions work meanwhile, " +
			"with only\nuv and the shome commands in them -- no git, no editor.")
	}
	return fmt.Errorf("build the session image: %w\n%s", err, firstLines(text, 5))
}

// firstLines trims a long failure to the part that names the problem.
func firstLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = append(lines[:n], "...")
	}
	return strings.Join(lines, "\n")
}

// PruneImages removes session images other than the one in use.
//
// The tag is derived from the package set, so every change to that set builds
// a new image and leaves the previous one behind -- around 320 MB each. On a
// machine somebody has lent to a cluster, quietly accumulating a few hundred
// megabytes per edit is not acceptable.
//
// Only shome's own images, matched by repository, and only ones no container
// is using: the runtime refuses to delete an image in use, which is exactly
// the right answer while a session is running on it.
func (r *Runtime) PruneImages(ctx context.Context, keep string) []string {
	out, err := exec.CommandContext(ctx, r.Bin, "image", "list").Output()
	if err != nil {
		return nil
	}
	var removed []string
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || f[0] != ImageRepo {
			continue
		}
		ref := f[0] + ":" + f[1]
		if ref == keep {
			continue
		}
		if err := exec.CommandContext(ctx, r.Bin, "image", "delete", ref).Run(); err == nil {
			removed = append(removed, ref)
		}
	}
	return removed
}
