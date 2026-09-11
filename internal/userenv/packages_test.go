package userenv

import (
	"strings"
	"testing"
)

// A package name is interpolated into a build file whose commands run as
// root inside the image, so anything that is not a package name is refused
// before it gets there.
func TestPackageNamesAreValidated(t *testing.T) {
	for _, bad := range []string{
		"git; rm -rf /", "git && curl evil", "$(whoami)", "`id`",
		"../etc/passwd", "a b", "UPPER", "-leading-dash", "x",
		strings.Repeat("a", 65), "pkg\nother",
	} {
		if _, err := Packages([]string{bad}); err == nil {
			t.Errorf("accepted %q as a package name", bad)
		}
	}
	for _, ok := range []string{
		"git", "git-lfs", "build-essential", "libssl3", "g++", "python3.12",
		"ca-certificates", "vim-tiny",
	} {
		if _, err := Packages([]string{ok}); err != nil {
			t.Errorf("rejected %q: %v", ok, err)
		}
	}
	// A blank is skipped rather than refused: the list is a hand-editable
	// file, and a trailing newline is not a mistake worth an error.
	got, err := Packages([]string{"", "  ", "tmux"})
	if err != nil {
		t.Fatalf("a blank line was treated as an error: %v", err)
	}
	for _, p := range got {
		if strings.TrimSpace(p) == "" {
			t.Error("a blank became a package")
		}
	}
}

// The base set is what shome guarantees, and an admin's additions are on top
// of it -- not instead of it. A script that works on one machine should work
// on the next.
func TestExtraPackagesAddToTheBaseAndCannotRemoveIt(t *testing.T) {
	got, err := Packages([]string{"tmux"})
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, p := range got {
		have[p] = true
	}
	for _, p := range BasePackages() {
		if !have[p] {
			t.Errorf("the base package %q is missing", p)
		}
	}
	if !have["tmux"] {
		t.Error("the addition was dropped")
	}
	// Duplicates collapse rather than appearing twice in the build.
	twice, _ := Packages([]string{"git", "git", "tmux"})
	n := 0
	for _, p := range twice {
		if p == "git" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("git appears %d times", n)
	}
}

// The things deliberately left out, so a later change has to be deliberate
// rather than accidental.
func TestBaseSetOmitsWhatItShould(t *testing.T) {
	have := map[string]bool{}
	for _, p := range BasePackages() {
		have[p] = true
	}
	for _, p := range []string{"python3", "python3-pip", "build-essential", "gcc", "tmux"} {
		if have[p] {
			t.Errorf("%q is in the base set; it was excluded on purpose "+
				"(uv provides interpreters, a compiler is 239 MB, and tmux "+
				"cannot outlive a session whose container is discarded)", p)
		}
	}
	// And the things nothing else works without.
	for _, p := range []string{"ca-certificates", "git", "gh", "less"} {
		if !have[p] {
			t.Errorf("%q is missing from the base set", p)
		}
	}
}
