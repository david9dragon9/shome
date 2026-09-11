package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/davidwu/shome/internal/agentapi"
	"github.com/davidwu/shome/internal/userfs"
)

// An account's file listing is sent when it changes, and not otherwise.
//
// It used to ride on every heartbeat -- ten a second -- which is why the cap
// on its size was 5,000 entries: smaller than one virtualenv, so an account
// that had installed anything had a truncated listing, and `shome fs cp` and
// `shome fs sync` work out what to copy from that listing.
func TestAStorageReportIsSentOncePerChange(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "users", "alice")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(home, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("a.txt", "one")

	a := &Agent{Files: userfs.New(root)}

	rep, start, ok := a.StorageReport()
	if !ok || len(rep) != 1 || rep[0].User != "alice" {
		t.Fatalf("first report = %+v, %v", rep, ok)
	}
	if start.IsZero() {
		t.Error("a report must say when its measurement began")
	}

	// Asked again immediately: the same measurement, already sent.
	if _, _, ok := a.StorageReport(); ok {
		t.Error("the same measurement was sent twice")
	}

	// A fresh measurement of an unchanged tree is also not worth sending.
	a.InvalidateStorage()
	if _, _, ok := a.StorageReport(); ok {
		t.Error("an unchanged listing was re-sent")
	}

	// A real change is sent.
	write("b.txt", "two")
	a.InvalidateStorage()
	rep, _, ok = a.StorageReport()
	if !ok {
		t.Fatal("a changed listing was not sent")
	}
	if len(rep) != 1 || len(rep[0].Entries) != 2 {
		t.Errorf("report = %+v, want both files", rep)
	}
}

// The fingerprint distinguishes the differences the controller would store.
func TestStorageFingerprintNoticesWhatMatters(t *testing.T) {
	base := []agentapi.UserStorage{{
		User: "alice", Bytes: 10, Files: 1,
		Entries: []agentapi.FileEntry{{Path: "a", Size: 10, MTime: 5}},
	}}
	same := []agentapi.UserStorage{{
		User: "alice", Bytes: 10, Files: 1,
		Entries: []agentapi.FileEntry{{Path: "a", Size: 10, MTime: 5}},
	}}
	if storageFingerprint(base) != storageFingerprint(same) {
		t.Error("two identical reports fingerprinted differently")
	}
	for name, changed := range map[string][]agentapi.UserStorage{
		"a renamed file": {{User: "alice", Bytes: 10, Files: 1,
			Entries: []agentapi.FileEntry{{Path: "b", Size: 10, MTime: 5}}}},
		"a resized file": {{User: "alice", Bytes: 11, Files: 1,
			Entries: []agentapi.FileEntry{{Path: "a", Size: 11, MTime: 5}}}},
		"a touched file": {{User: "alice", Bytes: 10, Files: 1,
			Entries: []agentapi.FileEntry{{Path: "a", Size: 10, MTime: 6}}}},
		"another account": {{User: "bob", Bytes: 10, Files: 1,
			Entries: []agentapi.FileEntry{{Path: "a", Size: 10, MTime: 5}}}},
		"a truncated listing": {{User: "alice", Bytes: 10, Files: 1, Truncated: true,
			Entries: []agentapi.FileEntry{{Path: "a", Size: 10, MTime: 5}}}},
	} {
		if storageFingerprint(base) == storageFingerprint(changed) {
			t.Errorf("%s did not change the fingerprint", name)
		}
	}
}
