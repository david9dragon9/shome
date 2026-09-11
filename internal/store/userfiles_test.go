package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func fsStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, context.Background()
}

func report(t *testing.T, s *Store, ctx context.Context, node, user string,
	files map[string]int64, truncated bool) {

	t.Helper()
	var list []UserFile
	var usage NodeUsage
	for p, n := range files {
		list = append(list, UserFile{Path: p, Size: n, MTime: time.Unix(1000, 0)})
		usage.Bytes += n
		usage.Files++
	}
	usage.Truncated = truncated
	if err := s.ReplaceNodeFiles(ctx, node, user, list, usage, time.Unix(2000, 0)); err != nil {
		t.Fatal(err)
	}
}

// A report is a machine's complete view, so anything it stops listing has
// gone. Merging instead would leave deleted files counting against a user
// forever.
func TestReplaceNodeFilesReplacesRatherThanMerges(t *testing.T) {
	s, ctx := fsStore(t)
	report(t, s, ctx, "mini", "alice", map[string]int64{"a": 10, "b": 20}, false)
	report(t, s, ctx, "mini", "alice", map[string]int64{"b": 20}, false)

	files, err := s.UserFiles(ctx, "alice", "", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Path != "b" {
		t.Fatalf("after a report omitting 'a', index has %+v", files)
	}
	if b, _, _ := mustTotal(t, s, ctx, "alice"); b != 20 {
		t.Errorf("total = %d, want 20", b)
	}
}

// One machine's report must not disturb another's -- they arrive
// independently and at different times.
func TestReplaceNodeFilesIsScopedToOneNodeAndUser(t *testing.T) {
	s, ctx := fsStore(t)
	report(t, s, ctx, "mini", "alice", map[string]int64{"a": 10}, false)
	report(t, s, ctx, "gpu", "alice", map[string]int64{"a": 30}, false)
	report(t, s, ctx, "mini", "bob", map[string]int64{"a": 5}, false)

	// Re-reporting mini/alice as empty leaves the other two alone.
	report(t, s, ctx, "mini", "alice", nil, false)

	if b, _, _ := mustTotal(t, s, ctx, "alice"); b != 30 {
		t.Errorf("alice total = %d, want 30 (gpu only)", b)
	}
	if b, _, _ := mustTotal(t, s, ctx, "bob"); b != 5 {
		t.Errorf("bob total = %d, want 5", b)
	}
}

// The cluster-wide figure is additive across machines: that is the whole
// premise of "a copy costs its size again".
func TestUserDiskTotalIsAdditiveAcrossMachines(t *testing.T) {
	s, ctx := fsStore(t)
	report(t, s, ctx, "mini", "alice", map[string]int64{"model.bin": 100}, false)
	report(t, s, ctx, "gpu", "alice", map[string]int64{"model.bin": 100}, false)
	report(t, s, ctx, "book", "alice", map[string]int64{"model.bin": 100}, false)

	bytes, files, _ := mustTotal(t, s, ctx, "alice")
	if bytes != 300 || files != 3 {
		t.Errorf("total = %d bytes / %d files, want 300 / 3 -- the same path on "+
			"three machines is three files", bytes, files)
	}

	byNode, err := s.UserDiskByNode(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(byNode) != 3 {
		t.Fatalf("UserDiskByNode = %+v, want three machines", byNode)
	}
	for _, n := range byNode {
		if n.Bytes != 100 {
			t.Errorf("%s = %d bytes, want 100", n.Node, n.Bytes)
		}
	}
}

// A departed machine's space is not the user's problem: leaving it counted
// would put them permanently over a limit they cannot get back under.
func TestForgetNodeStorageFreesTheAccount(t *testing.T) {
	s, ctx := fsStore(t)
	report(t, s, ctx, "mini", "alice", map[string]int64{"a": 10}, false)
	report(t, s, ctx, "gpu", "alice", map[string]int64{"a": 90}, false)

	if err := s.ForgetNodeStorage(ctx, "gpu"); err != nil {
		t.Fatal(err)
	}
	if b, _, _ := mustTotal(t, s, ctx, "alice"); b != 10 {
		t.Errorf("total after gpu left = %d, want 10", b)
	}
	if _, err := s.FindUserFile(ctx, "alice", "gpu", "a"); err == nil {
		t.Error("a departed machine's files are still in the index")
	}
}

func TestForgetUserStorage(t *testing.T) {
	s, ctx := fsStore(t)
	report(t, s, ctx, "mini", "alice", map[string]int64{"a": 10}, false)
	report(t, s, ctx, "mini", "bob", map[string]int64{"a": 10}, false)

	if err := s.ForgetUserStorage(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	if b, _, _ := mustTotal(t, s, ctx, "alice"); b != 0 {
		t.Errorf("alice total after deletion = %d, want 0", b)
	}
	if b, _, _ := mustTotal(t, s, ctx, "bob"); b != 10 {
		t.Errorf("bob total = %d, want 10 -- deleting alice must not touch bob", b)
	}
}

// A truncated listing must stay flagged, because the listing is then
// incomplete while the usage figure is not, and the UI has to say so.
func TestTruncatedFlagSurvives(t *testing.T) {
	s, ctx := fsStore(t)
	report(t, s, ctx, "mini", "alice", map[string]int64{"a": 1}, true)

	byNode, err := s.UserDiskByNode(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(byNode) != 1 || !byNode[0].Truncated {
		t.Fatalf("truncated flag lost: %+v", byNode)
	}
}

func TestUserFilesFiltersByNodeAndPrefix(t *testing.T) {
	s, ctx := fsStore(t)
	report(t, s, ctx, "mini", "alice", map[string]int64{"data/a": 1, "data/b": 1, "other": 1}, false)
	report(t, s, ctx, "gpu", "alice", map[string]int64{"data/a": 1}, false)

	all, err := s.UserFiles(ctx, "alice", "", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Errorf("everything, everywhere = %d files, want 4", len(all))
	}

	one, err := s.UserFiles(ctx, "alice", "mini", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(one) != 3 {
		t.Errorf("mini = %d files, want 3", len(one))
	}

	pre, err := s.UserFiles(ctx, "alice", "", "data/", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(pre) != 3 {
		t.Errorf("data/ = %d files, want 3 (two on mini, one on gpu)", len(pre))
	}
}

// An account may only ever see its own files, whatever it asks for.
func TestUserFilesNeverCrossesAccounts(t *testing.T) {
	s, ctx := fsStore(t)
	report(t, s, ctx, "mini", "alice", map[string]int64{"secret": 1}, false)

	files, err := s.UserFiles(ctx, "bob", "", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Errorf("bob sees %+v", files)
	}
	if _, err := s.FindUserFile(ctx, "bob", "mini", "secret"); err == nil {
		t.Error("bob can look up alice's file")
	}
}

func mustTotal(t *testing.T, s *Store, ctx context.Context, user string) (int64, int, error) {
	t.Helper()
	b, f, err := s.UserDiskTotal(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	return b, f, nil
}

// A transferred file is listed at once, without waiting for the machine's
// next report.
//
// The index is built from reports every twenty seconds, and for that whole
// window a copy that had demonstrably worked was "not in the index" -- so
// `shome fs cp mini:data gpu:` followed by anything that reads the listing
// failed on a file that was right there.
func TestNoteUserFileIndexesATransferredFileAtOnce(t *testing.T) {
	s, ctx := fsStore(t)
	report(t, s, ctx, "gpu-box", "alice", map[string]int64{"old.bin": 10}, false)

	if err := s.NoteUserFile(ctx, UserFile{
		Node: "gpu-box", User: "alice", Path: "data/new.bin",
		Size: 4096, MTime: time.Unix(3000, 0),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.UserFiles(ctx, "alice", "gpu-box", "data", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Path != "data/new.bin" || got[0].Size != 4096 {
		t.Fatalf("index has %+v, want data/new.bin at 4096 bytes", got)
	}

	// Usage totals are deliberately untouched: they are measured from the
	// filesystem by the machine that holds the file, and a limit enforced
	// against anything else eventually locks somebody out of free space.
	if b, _, _ := mustTotal(t, s, ctx, "alice"); b != 10 {
		t.Errorf("total = %d, want the last measured 10 -- not adjusted by hand", b)
	}

	// Writing it again is an update, not a duplicate: the same transfer can
	// be reported twice if the acknowledgement is lost.
	if err := s.NoteUserFile(ctx, UserFile{
		Node: "gpu-box", User: "alice", Path: "data/new.bin",
		Size: 8192, MTime: time.Unix(3001, 0),
	}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.UserFiles(ctx, "alice", "gpu-box", "data", 100)
	if len(got) != 1 || got[0].Size != 8192 {
		t.Fatalf("index has %+v, want one row of 8192 bytes", got)
	}

	// And the far side of a move stops being listed where it no longer is.
	if err := s.ForgetUserFile(ctx, "gpu-box", "alice", "old.bin"); err != nil {
		t.Fatal(err)
	}
	all, _ := s.UserFiles(ctx, "alice", "gpu-box", "", 100)
	for _, f := range all {
		if f.Path == "old.bin" {
			t.Error("a moved file is still listed on the machine it left")
		}
	}

	// Nothing incomplete gets in: a row with no node or path would be
	// unreachable and unremovable.
	for _, f := range []UserFile{
		{User: "alice", Path: "x"}, {Node: "n", Path: "x"}, {Node: "n", User: "alice"},
	} {
		if err := s.NoteUserFile(ctx, f); err != nil {
			t.Errorf("NoteUserFile(%+v) = %v, want it ignored", f, err)
		}
	}
}
