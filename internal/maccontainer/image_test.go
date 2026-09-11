package maccontainer

import (
	"sort"
	"strings"
	"testing"
)

// The tag names what is in the image, so changing the set builds a new one
// and leaving it alone reuses what is there.
func TestTagTracksContents(t *testing.T) {
	a := []string{"ca-certificates", "curl", "git"}
	b := []string{"ca-certificates", "curl", "git", "tmux"}
	base := "debian:trixie"

	if ImageTag(base, a) == ImageTag(base, b) {
		t.Error("adding a package did not change the tag, so the image would not rebuild")
	}
	if ImageTag(base, a) != ImageTag(base, a) {
		t.Error("the tag is not stable")
	}
	// Order must not matter, because the caller sorts before building --
	// see userenv.Packages -- and a reordered file must not trigger a
	// pointless rebuild.
	sorted := append([]string{}, a...)
	sort.Strings(sorted)
	if ImageTag(base, a) != ImageTag(base, sorted) {
		t.Error("reordering the set changed the tag")
	}
	// A different base is a different image.
	if ImageTag(base, a) == ImageTag("other:latest", a) {
		t.Error("the base image is not part of the tag")
	}
	if !strings.HasPrefix(ImageTag(base, a), ImageRepo+":") {
		t.Errorf("tag %q is not in shome's own repository", ImageTag(base, a))
	}
}

func TestContainerfileInstallsEverythingAndCleansUp(t *testing.T) {
	pkgs := []string{"ca-certificates", "git", "tmux"}
	f := Containerfile("debian:trixie", pkgs)

	if !strings.Contains(f, "FROM debian:trixie") {
		t.Error("no FROM line")
	}
	for _, p := range pkgs {
		if !strings.Contains(f, p) {
			t.Errorf("%q is not installed by the build file", p)
		}
	}
	// Recommends pull in man pages and X libraries for a machine nobody
	// looks at; the apt lists are tens of megabytes a session never reads.
	if !strings.Contains(f, "--no-install-recommends") {
		t.Error("recommends are not suppressed")
	}
	if !strings.Contains(f, "rm -rf /var/lib/apt/lists") {
		t.Error("the apt lists are left in the image")
	}
	if !strings.Contains(f, "DEBIAN_FRONTEND=noninteractive") {
		t.Error("apt could stop for a prompt during a build nobody is watching")
	}
}
