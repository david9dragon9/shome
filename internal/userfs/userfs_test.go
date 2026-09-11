package userfs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, s *Store, user, rel, body string) {
	t.Helper()
	if _, err := s.Put(user, rel, strings.NewReader(body), -1); err != nil {
		t.Fatalf("Put(%q): %v", rel, err)
	}
}

// The one test that matters most: a user must not be able to name a path
// belonging to anyone else, or anything outside the store at all.
func TestResolveRefusesEscapes(t *testing.T) {
	s := New(t.TempDir())
	base := s.UserDir("alice")

	for _, rel := range []string{
		"../bob/secret",
		"../../etc/passwd",
		"a/../../bob/secret",
		"a/b/../../../bob",
		"..",
		"./../bob",
		"a/./../../bob",
	} {
		got, err := s.Resolve("alice", rel)
		if err == nil {
			t.Errorf("Resolve(%q) = %q, want an error", rel, got)
		}
	}

	// And the paths that should work, still do.
	for rel, want := range map[string]string{
		"":            base,
		".":           base,
		"f":           filepath.Join(base, "f"),
		"a/b/c":       filepath.Join(base, "a/b/c"),
		"a/../b":      filepath.Join(base, "b"),
		"./a":         filepath.Join(base, "a"),
		"a/b/../../c": filepath.Join(base, "c"),
		// A leading slash means "the top of my storage", there being no other
		// filesystem a cluster user can name.
		"/etc/passwd": filepath.Join(base, "etc/passwd"),
		"/data":       filepath.Join(base, "data"),
	} {
		got, err := s.Resolve("alice", rel)
		if err != nil {
			t.Errorf("Resolve(%q): %v", rel, err)
			continue
		}
		if got != want {
			t.Errorf("Resolve(%q) = %q, want %q", rel, got, want)
		}
	}
}

// A user name is part of a filesystem path, so it gets the same treatment as
// the relative path does.
func TestValidUserRefusesPathParts(t *testing.T) {
	for _, u := range []string{"", ".", "..", "a/b", "a/../b", "/abs", `back\slash`} {
		if err := ValidUser(u); err == nil {
			t.Errorf("ValidUser(%q) = nil, want an error", u)
		}
	}
	for _, u := range []string{"alice", "bob-1", "user_2", "a.b"} {
		if err := ValidUser(u); err != nil {
			t.Errorf("ValidUser(%q): %v", u, err)
		}
	}
}

func TestPutEnforcesLimit(t *testing.T) {
	s := New(t.TempDir())

	if _, err := s.Put("alice", "big", strings.NewReader("0123456789"), 4); err == nil {
		t.Fatal("Put over the limit succeeded, want an error")
	}
	// The rename happens last, so a rejected upload must leave nothing behind
	// -- not a partial file, and not a stray temp file counting against them.
	if _, err := os.Stat(filepath.Join(s.UserDir("alice"), "big")); !os.IsNotExist(err) {
		t.Errorf("rejected upload left a file behind: %v", err)
	}
	des, _ := os.ReadDir(s.UserDir("alice"))
	if len(des) != 0 {
		t.Errorf("rejected upload left %d entries: %v", len(des), des)
	}

	// Exactly at the limit is allowed; a limit is a maximum, not a bound.
	if n, err := s.Put("alice", "exact", strings.NewReader("0123"), 4); err != nil || n != 4 {
		t.Fatalf("Put at the limit: n=%d err=%v", n, err)
	}

	// Zero means nothing may be stored, and must not be read as unlimited.
	if _, err := s.Put("alice", "zero", strings.NewReader("x"), 0); err == nil {
		t.Error("Put with a zero limit succeeded, want an error")
	}
	if _, err := s.Put("alice", "unl", strings.NewReader("x"), -1); err != nil {
		t.Errorf("Put with an unlimited limit: %v", err)
	}
}

// A failed overwrite must not destroy the file it was replacing.
func TestPutKeepsPreviousVersionOnFailure(t *testing.T) {
	s := New(t.TempDir())
	write(t, s, "alice", "f", "original")

	if _, err := s.Put("alice", "f", strings.NewReader("far too long"), 3); err == nil {
		t.Fatal("oversized overwrite succeeded, want an error")
	}
	b, err := os.ReadFile(filepath.Join(s.UserDir("alice"), "f"))
	if err != nil || string(b) != "original" {
		t.Fatalf("after a failed overwrite: %q %v, want %q", b, err, "original")
	}
}

func TestWalkCountsEverythingButListsAtMostMax(t *testing.T) {
	s := New(t.TempDir())
	for _, f := range []string{"a", "b", "d/c", "d/e/f"} {
		write(t, s, "alice", f, "12345")
	}

	entries, usage, truncated, err := s.Walk("alice", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || !truncated {
		t.Errorf("entries=%d truncated=%v, want 2 and true", len(entries), truncated)
	}
	// The usage figure is what a limit is enforced against, so it must cover
	// every file even when the listing is capped.
	if usage.Files != 4 || usage.Bytes != 20 {
		t.Errorf("usage = %d files / %d bytes, want 4 / 20", usage.Files, usage.Bytes)
	}
	// alice/ + d/ + d/e/ + 4 files
	if usage.Inodes != 7 {
		t.Errorf("usage.Inodes = %d, want 7", usage.Inodes)
	}

	_, _, truncated, err = s.Walk("alice", 0)
	if err != nil || truncated {
		t.Errorf("Walk with no cap: truncated=%v err=%v", truncated, err)
	}
}

func TestWalkOfAnUnusedAccountIsEmptyNotAnError(t *testing.T) {
	s := New(t.TempDir())
	entries, usage, _, err := s.Walk("nobody", 100)
	if err != nil {
		t.Fatalf("Walk of an unused account: %v", err)
	}
	if len(entries) != 0 || usage.Bytes != 0 {
		t.Errorf("got %d entries / %d bytes, want 0 / 0", len(entries), usage.Bytes)
	}
}

func TestRemove(t *testing.T) {
	s := New(t.TempDir())
	write(t, s, "alice", "keep", "x")
	write(t, s, "alice", "dir/f", "x")

	if err := s.Remove("alice", "", true); err == nil {
		t.Error(`Remove("") succeeded; it must refuse to delete the whole account`)
	}
	if err := s.Remove("alice", ".", true); err == nil {
		t.Error(`Remove(".") succeeded; it must refuse to delete the whole account`)
	}
	if _, err := os.Stat(filepath.Join(s.UserDir("alice"), "keep")); err != nil {
		t.Fatalf("a refused Remove deleted files anyway: %v", err)
	}

	if err := s.Remove("alice", "dir", false); err == nil {
		t.Error("Remove of a directory without -r succeeded, want an error")
	}
	if err := s.Remove("alice", "dir", true); err != nil {
		t.Errorf("Remove -r: %v", err)
	}
	if err := s.Remove("alice", "gone", false); err == nil {
		t.Error("Remove of a missing path succeeded, want an error")
	}
	if err := s.Remove("alice", "../bob", true); err == nil {
		t.Error("Remove of another account's path succeeded, want an error")
	}
}

// Two accounts on one machine are two directories, and neither sees the
// other's usage.
func TestAccountsAreSeparate(t *testing.T) {
	s := New(t.TempDir())
	write(t, s, "alice", "f", "aaaa")
	write(t, s, "bob", "f", "bb")

	au, err := s.UsageFor("alice")
	if err != nil {
		t.Fatal(err)
	}
	bu, err := s.UsageFor("bob")
	if err != nil {
		t.Fatal(err)
	}
	if au.Bytes != 4 || bu.Bytes != 2 {
		t.Errorf("alice=%d bob=%d, want 4 and 2", au.Bytes, bu.Bytes)
	}

	users, err := s.Users()
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 2 {
		t.Errorf("Users() = %v, want alice and bob", users)
	}
}
