package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A real Slurm install on the same machine must survive `shome nuke`
// untouched. Removing someone's actual sbatch would be a serious bug, and the
// only thing separating the two cases is where the symlink points.
func TestFindShimsLeavesRealSlurmAlone(t *testing.T) {
	dir := t.TempDir()
	self := filepath.Join(dir, "shome")
	os.WriteFile(self, []byte("#!/bin/sh\n"), 0o755)

	// Ours: a symlink to the running binary.
	if err := os.Symlink(self, filepath.Join(dir, "sbatch")); err != nil {
		t.Fatal(err)
	}
	// Not ours: a symlink to something else entirely.
	other := filepath.Join(dir, "slurm-real")
	os.WriteFile(other, []byte("#!/bin/sh\n"), 0o755)
	if err := os.Symlink(other, filepath.Join(dir, "squeue")); err != nil {
		t.Fatal(err)
	}
	// Also not ours: a plain file, not a symlink at all.
	os.WriteFile(filepath.Join(dir, "sinfo"), []byte("#!/bin/sh\n"), 0o755)

	t.Setenv("PATH", dir)
	found := findShims(self)

	if len(found) != 1 {
		t.Fatalf("found %v, want only the shome-owned sbatch", found)
	}
	if filepath.Base(found[0]) != "sbatch" {
		t.Errorf("found %q, want the sbatch symlink", found[0])
	}
	for _, f := range found {
		if strings.HasSuffix(f, "squeue") || strings.HasSuffix(f, "sinfo") {
			t.Errorf("%q is not shome's and must not be removed", f)
		}
	}
}

// A shim pointing at a DIFFERENT shome installation belongs to that one.
// Matching on the link's name rather than its target would make uninstalling
// one copy break another.
func TestFindShimsIgnoresAnotherInstallsShims(t *testing.T) {
	dir := t.TempDir()
	mine := filepath.Join(dir, "shome")
	theirs := filepath.Join(dir, "shome-elsewhere")
	os.WriteFile(mine, []byte("a"), 0o755)
	os.WriteFile(theirs, []byte("b"), 0o755)
	if err := os.Symlink(theirs, filepath.Join(dir, "sbatch")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	if found := findShims(mine); len(found) != 0 {
		t.Errorf("found %v; those shims belong to another installation", found)
	}
}

func TestFindShimsIgnoresEmptyPath(t *testing.T) {
	t.Setenv("PATH", "")
	if got := findShims("/nonexistent/shome"); len(got) != 0 {
		t.Errorf("findShims with no PATH returned %v", got)
	}
}

// An uninstaller removes itself, not everything that resembles it. A second
// shome the user deliberately keeps -- or an unrelated program of the same
// name -- must be left alone even when it is on PATH.
func TestFindBinariesRemovesOnlyThisInstall(t *testing.T) {
	mine := t.TempDir()
	other := t.TempDir()
	self := filepath.Join(mine, "shome")
	os.WriteFile(self, []byte("binary"), 0o755)
	os.WriteFile(filepath.Join(mine, "shome-shell"), []byte("binary"), 0o755)
	os.WriteFile(filepath.Join(other, "shome"), []byte("someone else's"), 0o755)

	t.Setenv("PATH", mine+string(os.PathListSeparator)+other)
	found := findBinaries(self)

	if len(found) != 2 {
		t.Fatalf("found %v, want this install's shome and shome-shell", found)
	}
	for _, f := range found {
		if strings.HasPrefix(f, other) {
			t.Errorf("%q belongs to another installation and must survive", f)
		}
	}
}

// shome-shell is optional: a worker node never has one.
func TestFindBinariesWithoutShomeShell(t *testing.T) {
	dir := t.TempDir()
	self := filepath.Join(dir, "shome")
	os.WriteFile(self, []byte("binary"), 0o755)
	want := resolve(self)
	if found := findBinaries(self); len(found) != 1 || found[0] != want {
		t.Errorf("found %v, want just %q", found, want)
	}
}

func TestFindProfileMentions(t *testing.T) {
	home := t.TempDir()
	os.WriteFile(filepath.Join(home, ".zshrc"), []byte("export PATH=$HOME/.local/bin:$PATH # shome\n"), 0o644)
	os.WriteFile(filepath.Join(home, ".bashrc"), []byte("alias ll='ls -l'\n"), 0o644)

	found := findProfileMentions(home)
	if len(found) != 1 || filepath.Base(found[0]) != ".zshrc" {
		t.Fatalf("found %v, want only .zshrc", found)
	}
}

// Shell startup files are reported, never edited: a bad automated edit there
// breaks every new terminal the user opens.
func TestProfilesAreNeverModified(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".profile")
	original := "export PATH=$HOME/.local/bin:$PATH # added by shome\n"
	os.WriteFile(path, []byte(original), 0o644)

	findProfileMentions(home)

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != original {
		t.Errorf("profile was modified:\n got %q\nwant %q", after, original)
	}
}

func TestHumanSizes(t *testing.T) {
	for _, tc := range []struct {
		in   int64
		want string
	}{
		{512, "512 B"},
		{2048, "2 KiB"},
		{5 << 20, "5 MiB"},
		{3 << 30, "3.0 GiB"},
	} {
		if got := human(tc.in); got != tc.want {
			t.Errorf("human(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSizeOfDirectorySumsFiles(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a"), make([]byte, 100), 0o644)
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, "sub", "b"), make([]byte, 200), 0o644)

	fi, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := sizeOf(dir, fi); got != 300 {
		t.Errorf("sizeOf = %d, want 300", got)
	}
}

// ~/.shome is shared between roles: a controller and an agent on the same
// machine each keep a pointer there. Removing the directory wholesale took the
// other role's pointer with it, leaving a running controller the CLI could no
// longer find.
func TestSurveyLeavesAnotherRolesPointerAlone(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dot := filepath.Join(home, ".shome")
	os.MkdirAll(dot, 0o700)

	root := filepath.Join(t.TempDir(), "shome-mine")
	os.MkdirAll(root, 0o700)
	t.Setenv("SHOME_ROOT", root)

	// Ours, and someone else's.
	os.WriteFile(filepath.Join(dot, "node"), []byte(root+"\n"), 0o600)
	os.WriteFile(filepath.Join(dot, "controller"), []byte("/Users/Shared/shome-other\n"), 0o600)

	p := survey(true)
	var paths []string
	for _, it := range p.items {
		paths = append(paths, it.path)
	}
	joined := strings.Join(paths, "\n")
	if !strings.Contains(joined, filepath.Join(dot, "node")) {
		t.Errorf("our own node pointer was not scheduled for removal:\n%s", joined)
	}
	if strings.Contains(joined, filepath.Join(dot, "controller")) {
		t.Errorf("another role's controller pointer must survive:\n%s", joined)
	}
	// And never the whole directory.
	for _, pth := range paths {
		if pth == dot {
			t.Errorf("~/.shome was scheduled for wholesale removal")
		}
	}
}

// The agent binaries a controller serves to joining machines live in
// ~/.shome/dist. A worker never serves them, so a worker uninstalling itself
// on a machine that also runs a controller must leave them alone -- removing
// them breaks every subsequent `shome invite` with a 404 on the download.
func TestWorkerNukeKeepsServedAgentBinaries(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dist := filepath.Join(home, ".shome", "dist", "linux-amd64")
	os.MkdirAll(dist, 0o755)
	os.WriteFile(filepath.Join(dist, "shome"), []byte("agent"), 0o755)

	root := filepath.Join(t.TempDir(), "shome-worker")
	os.MkdirAll(root, 0o700)
	t.Setenv("SHOME_ROOT", root)
	// Mark this root as an agent, which is what makes dist not ours.
	os.WriteFile(filepath.Join(root, "node.json"),
		[]byte(`{"role":"agent","node":"w","controller":"10.0.0.5:7817"}`), 0o600)

	for _, it := range survey(true).items {
		if strings.Contains(it.path, "dist") {
			t.Fatalf("a worker's uninstall scheduled %q for removal", it.path)
		}
	}

	// A controller, by contrast, does own them.
	os.WriteFile(filepath.Join(root, "node.json"),
		[]byte(`{"role":"controller","node":"c"}`), 0o600)
	found := false
	for _, it := range survey(true).items {
		if strings.Contains(it.path, "dist") {
			found = true
		}
	}
	if !found {
		t.Error("a controller's uninstall should remove the binaries it served")
	}
}
