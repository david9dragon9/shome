package platform

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// OrphanScratchIn lists job working directories in root that no live job owns.
//
// Shared by the platform backends because the layout is the same on both:
// one directory per job, named job-<id>. Identifying them by name means this
// still works when every other record of the job is gone, which is the
// situation it exists for.
func OrphanScratchIn(root string, live []int64) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // nothing has run here yet
		}
		return nil, err
	}
	alive := make(map[int64]bool, len(live))
	for _, id := range live {
		alive[id] = true
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "job-") {
			continue
		}
		id, err := strconv.ParseInt(strings.TrimPrefix(e.Name(), "job-"), 10, 64)
		if err != nil {
			// Not a job directory despite the prefix. Left alone: removing
			// something whose purpose is unknown is worse than leaving it.
			continue
		}
		if alive[id] {
			continue
		}
		out = append(out, filepath.Join(root, e.Name()))
	}
	return out, nil
}
