package platform

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func TestOrphanScratchIn(t *testing.T) {
	root := t.TempDir()
	for _, n := range []string{"job-1", "job-2", "job-42", "job-notanumber", "notajob", "job-7"} {
		if err := os.MkdirAll(filepath.Join(root, n), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// A stray file, which must not be mistaken for a job directory.
	os.WriteFile(filepath.Join(root, "job-99"), []byte("x"), 0o600)

	got, err := OrphanScratchIn(root, []int64{2, 7})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, g := range got {
		names = append(names, filepath.Base(g))
	}
	sort.Strings(names)
	want := []string{"job-1", "job-42"}
	if len(names) != len(want) {
		t.Fatalf("got %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("got %v, want %v", names, want)
		}
	}
}

// A live job's directory must never be swept: it is being written to.
func TestOrphanScratchSkipsLiveJobs(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "job-5"), 0o700)
	got, err := OrphanScratchIn(root, []int64{5})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("a running job's directory was listed for removal: %v", got)
	}
}

// A directory whose purpose cannot be determined is left alone. Removing
// something unrecognised is worse than leaving it.
func TestOrphanScratchLeavesUnrecognisedDirs(t *testing.T) {
	root := t.TempDir()
	for _, n := range []string{"job-", "job-abc", "cache", "tmp"} {
		os.MkdirAll(filepath.Join(root, n), 0o700)
	}
	got, err := OrphanScratchIn(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("unrecognised directories were listed for removal: %v", got)
	}
}

// A machine that has never run a job has no jobs root, which is not an error.
func TestOrphanScratchMissingRoot(t *testing.T) {
	got, err := OrphanScratchIn(filepath.Join(t.TempDir(), "never-created"), nil)
	if err != nil {
		t.Errorf("a missing jobs root should not be an error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v", got)
	}
}
