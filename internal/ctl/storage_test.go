package ctl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveRefusesEscape(t *testing.T) {
	s := NewStorage(t.TempDir())
	for _, bad := range []string{
		"../bob/secret", "../../etc/passwd", "a/../../../etc/passwd", "..",
	} {
		if got, err := s.Resolve("alice", bad); err == nil {
			t.Errorf("Resolve(%q) allowed, resolved to %q; it must be refused", bad, got)
		}
	}
	// Legitimate nested paths must still work.
	for _, ok := range []string{"data.txt", "a/b/c.txt", "./x", ""} {
		if _, err := s.Resolve("alice", ok); err != nil {
			t.Errorf("Resolve(%q) refused: %v", ok, err)
		}
	}
	// A leading slash is rerooted into the user's own area rather than
	// refused: there is no other filesystem a cluster user can name, so it
	// can only have meant the top of their storage.
	got, err := s.Resolve("alice", "/etc/passwd")
	if err != nil {
		t.Fatalf(`Resolve("/etc/passwd"): %v`, err)
	}
	if want := filepath.Join(s.UserDir("alice"), "etc/passwd"); got != want {
		t.Errorf("Resolve(\"/etc/passwd\") = %q, want %q", got, want)
	}
}

func TestResolveKeepsUsersApart(t *testing.T) {
	s := NewStorage(t.TempDir())
	a, _ := s.Resolve("alice", "f.txt")
	b, _ := s.Resolve("bob", "f.txt")
	if a == b {
		t.Fatal("two users resolved to the same path")
	}
	if !strings.Contains(a, "alice") || !strings.Contains(b, "bob") {
		t.Errorf("paths do not include the owner: %q %q", a, b)
	}
}

// Put is bounded by a budget -- how many more bytes the account may store
// anywhere -- rather than by an absolute cap on this directory. The caller
// computes it, because only the controller can see every machine and this is
// one of several holding the same allowance.
func TestPutRespectsTheBudgetItIsGiven(t *testing.T) {
	s := NewStorage(t.TempDir())

	if err := s.Put("alice", "small.bin", strings.NewReader(strings.Repeat("x", 512)), 1024); err != nil {
		t.Fatalf("write within the budget failed: %v", err)
	}
	// A write larger than the remaining budget must be refused.
	err := s.Put("alice", "big.bin", strings.NewReader(strings.Repeat("x", 1024)), 512)
	if err == nil {
		t.Fatal("write past the budget was allowed")
	}
	if !strings.Contains(err.Error(), "storage limit") {
		t.Errorf("unhelpful error: %v", err)
	}
	// And it must not have left a partial file behind.
	dir := s.UserDir("alice")
	if _, err := os.Stat(filepath.Join(dir, "big.bin")); err == nil {
		t.Error("refused upload left a partial file")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".upload-") {
			t.Errorf("temp file %s left behind", e.Name())
		}
	}

	// Exactly at the budget is allowed; a budget is a maximum, not a bound.
	if err := s.Put("alice", "exact.bin", strings.NewReader(strings.Repeat("x", 64)), 64); err != nil {
		t.Errorf("write of exactly the budget refused: %v", err)
	}
	// Zero means nothing more may be stored, and must not read as unlimited.
	if err := s.Put("alice", "zero.bin", strings.NewReader("x"), 0); err == nil {
		t.Error("write with a zero budget was allowed")
	}
	// Negative means unlimited.
	if err := s.Put("alice", "unl.bin", strings.NewReader(strings.Repeat("x", 4096)), -1); err != nil {
		t.Errorf("write with an unlimited budget refused: %v", err)
	}
}

func TestQuotaCannotBeBeatenByLyingAboutSize(t *testing.T) {
	s := NewStorage(t.TempDir())
	// A stream with no declared length must still be cut off at the quota.
	err := s.Put("alice", "stream.bin", strings.NewReader(strings.Repeat("y", 100_000)), 4096)
	if err == nil {
		t.Fatal("oversized stream was accepted")
	}
	used, _ := s.Usage("alice")
	if used > 4096 {
		t.Errorf("usage is %d bytes, over the 4096 quota", used)
	}
}

func TestUnlimitedQuota(t *testing.T) {
	s := NewStorage(t.TempDir())
	if err := s.Put("alice", "f.bin", strings.NewReader(strings.Repeat("z", 50_000)), -1); err != nil {
		t.Fatalf("quota 0 should mean unlimited: %v", err)
	}
}

func TestUsageAndRemove(t *testing.T) {
	s := NewStorage(t.TempDir())
	s.Put("alice", "a/one.txt", strings.NewReader("12345"), -1)
	s.Put("alice", "a/two.txt", strings.NewReader("123"), -1)
	got, err := s.Usage("alice")
	if err != nil {
		t.Fatal(err)
	}
	if got != 8 {
		t.Errorf("usage = %d, want 8", got)
	}
	if err := s.Remove("alice", "a/one.txt"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Usage("alice"); got != 3 {
		t.Errorf("usage after remove = %d, want 3", got)
	}
	// Wiping the whole area by accident must not be possible.
	if err := s.Remove("alice", ""); err == nil {
		t.Error("removing the entire storage area should be refused")
	}
}

func TestListIsScopedToTheUser(t *testing.T) {
	s := NewStorage(t.TempDir())
	s.Put("alice", "mine.txt", strings.NewReader("a"), -1)
	s.Put("bob", "theirs.txt", strings.NewReader("b"), -1)
	es, err := s.List("alice", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 1 || es[0].Name != "mine.txt" {
		t.Errorf("alice sees %+v; should see only her own file", es)
	}
}

// Zero means nothing may be stored, and negative means unlimited. These two
// used to be confused -- zero meant unlimited here while meaning none-allowed
// in the QoS layer -- which turned the strictest possible setting into the
// loosest.
func TestStorageQuotaZeroRefusesEverything(t *testing.T) {
	s := NewStorage(t.TempDir())
	if err := s.Put("alice", "x.txt", strings.NewReader("a"), 0); err == nil {
		t.Fatal("a zero quota accepted a write")
	}
	if err := s.Put("alice", "y.txt", strings.NewReader("a"), -1); err != nil {
		t.Fatalf("a negative quota should be unlimited: %v", err)
	}
}

// Regression: starting a throwaway cluster with SHOME_ROOT set used to
// rewrite this machine's pointer files, silently redirecting every later
// command -- including `shome nuke` -- at the temporary directory instead of
// the real installation.
func TestExplicitRootDoesNotRewriteTheMachinesPointers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	t.Setenv("SHOME_ROOT", "")
	WriteControllerPointer("/real/installation")
	WriteNodePointer("/real/installation")

	t.Setenv("SHOME_ROOT", "/tmp/throwaway")
	if RootIsExplicit() != true {
		t.Fatal("RootIsExplicit() = false with SHOME_ROOT set")
	}
	WriteControllerPointer("/tmp/throwaway")
	WriteNodePointer("/tmp/throwaway")

	for _, kind := range []string{"controller", "node"} {
		b, err := os.ReadFile(filepath.Join(home, ".shome", kind))
		if err != nil {
			t.Fatalf("read %s pointer: %v", kind, err)
		}
		if got := strings.TrimSpace(string(b)); got != "/real/installation" {
			t.Errorf("%s pointer = %q; an explicitly-rooted run overwrote it", kind, got)
		}
	}
}
