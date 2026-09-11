package job

import "testing"

// The working directory a job gets is writable, so a --chdir that climbs out
// of the account's storage would be a way to write outside the one tree the
// account may write to. Refused, not silently reinterpreted: collapsing
// "../elsewhere" into "elsewhere" would run the job somewhere the submitter
// did not name.
func TestCleanChdirRefusesWhatLeavesTheAccountsStorage(t *testing.T) {
	for _, bad := range []string{
		"..", "../elsewhere", "a/../../elsewhere", "../../../../etc",
		// Absolute means nothing on another machine: a cluster has no
		// shared filesystem.
		"/etc", "/home/alice@mini/work",
	} {
		if got, err := CleanChdir(bad); err == nil {
			t.Errorf("CleanChdir(%q) = %q, want an error", bad, got)
		}
	}
}

func TestCleanChdirAcceptsWhatStaysInside(t *testing.T) {
	for in, want := range map[string]string{
		"":               "",
		"   ":            "",
		".":              "",
		"runs":           "runs",
		"runs/2026-09":   "runs/2026-09",
		"./runs/2026-09": "runs/2026-09",
		"a/../b":         "b",
		"runs//2026-09/": "runs/2026-09",
	} {
		got, err := CleanChdir(in)
		if err != nil {
			t.Errorf("CleanChdir(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("CleanChdir(%q) = %q, want %q", in, got, want)
		}
	}
}
