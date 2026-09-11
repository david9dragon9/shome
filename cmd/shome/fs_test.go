package main

import (
	"os"
	"path/filepath"
	"testing"
)

// A local path keeps the spelling it was given.
//
// The index form strips the leading separator so a path can be matched
// against what machines reported. Applying that to a local path turned
// /tmp/data into tmp/data -- a different directory, usually an empty one --
// so a sync from an absolute path reported "no files".
func TestLocalRefKeepsItsSpelling(t *testing.T) {
	r, err := parseRef("/tmp/data")
	if err != nil {
		t.Fatal(err)
	}
	if r.Node != "" {
		t.Errorf("a path with no colon named machine %q", r.Node)
	}
	if r.Local != "/tmp/data" {
		t.Errorf("Local = %q, want the path as typed", r.Local)
	}
	if r.Path != "tmp/data" {
		t.Errorf("Path = %q, want the index form", r.Path)
	}
	if got := r.String(); got != "/tmp/data" {
		t.Errorf("String = %q, want the path as typed", got)
	}
	n, err := parseRef("mini:data/x")
	if err != nil {
		t.Fatal(err)
	}
	if n.Node != "mini" || n.Path != "data/x" || n.Local != "" {
		t.Errorf("parseRef(mini:data/x) = %+v", n)
	}
}

// A tree is listed as relative paths, sorted, with symlinks counted rather
// than followed.
func TestLocalTreeListsRegularFilesOnly(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "nested", "deep"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"b.txt", "nested/a.txt", "nested/deep/c.bin"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("xyz"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("/etc/hosts", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	files, total, links, err := localTree(dir)
	if err != nil {
		t.Fatal(err)
	}
	if links != 1 {
		t.Errorf("links = %d, want 1 -- a symlink must be counted, not followed", links)
	}
	if total != 9 {
		t.Errorf("total = %d, want 9", total)
	}
	want := []string{"b.txt", "nested/a.txt", "nested/deep/c.bin"}
	if len(files) != len(want) {
		t.Fatalf("files = %+v, want %v", files, want)
	}
	for i, w := range want {
		if files[i].Rel != w {
			t.Errorf("files[%d].Rel = %q, want %q (sorted, forward slashes)", i, files[i].Rel, w)
		}
	}
}

// Where a directory copy lands, which follows cp -r.
func TestTreeDestFollowsCpSemantics(t *testing.T) {
	cases := []struct {
		dir, arg string
		dst      ref
		want     string
	}{
		// A bare machine means the directory itself at the top.
		{"/tmp/data", "", ref{Node: "mini"}, "data"},
		// A trailing separator means inside that directory.
		{"/tmp/data", "backups/", ref{Node: "mini", Path: "backups/"}, "backups/data"},
		// A named path renames it.
		{"/tmp/data", "other", ref{Node: "mini", Path: "other"}, "other"},
		// A trailing separator on the source does not change the name.
		{"/tmp/data/", "", ref{Node: "mini"}, "data"},
	}
	for _, c := range cases {
		if got := treeDest(c.dir, c.dst, c.arg); got.Path != c.want {
			t.Errorf("treeDest(%q, %+v, %q) = %q, want %q", c.dir, c.dst, c.arg, got.Path, c.want)
		}
	}
}
