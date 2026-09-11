package platform

import (
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Disk and inode accounting.
//
// Two different numbers, both needed:
//
//   - What the filesystem has left. A machine's owner cares about this even if
//     shome is using almost none of it, because something else filling the
//     disk is still shome's problem the moment a job fails for want of space.
//   - What shome itself is using. That is what an owner's contribution cap is
//     a cap on, and it is the only figure shome can honestly promise to bound.
//
// Inodes are tracked alongside bytes because they run out independently. A job
// writing a million empty files uses no meaningful space and can still make a
// filesystem unusable, and a cap in gigabytes says nothing about it.

// FSStat is what a filesystem reports about itself.
type FSStat struct {
	TotalBytes  int64
	FreeBytes   int64
	TotalInodes int64
	FreeInodes  int64
}

// UsedBytes is space in use by everything on the filesystem.
func (f FSStat) UsedBytes() int64 { return f.TotalBytes - f.FreeBytes }

// TreeUsage is what one directory tree occupies.
type TreeUsage struct {
	Bytes  int64
	Inodes int64 // files plus directories, which is what the kernel counts
	At     time.Time
}

// treeCache memoises a tree walk.
//
// Walking a job scratch tree costs one stat per file, and this is read on
// every heartbeat. Without a cache a node with a large working set would spend
// its time measuring itself instead of running work -- and the number does not
// change fast enough to be worth that.
// Keyed by path. A single entry was the first attempt and was wrong: two
// backends measuring different roots in one process -- a controller and a
// worker under test, say -- would each be handed the other's figure, which
// reads as a plausible number rather than as a bug.
type treeCache struct {
	mu     sync.Mutex
	byRoot map[string]TreeUsage
	ttl    time.Duration
}

// TreeTTL is how long a measurement is reused. Chosen against the heartbeat
// interval: long enough that consecutive heartbeats share one walk, short
// enough that a cap is noticed within seconds of being crossed.
const TreeTTL = 10 * time.Second

var shomeTree = &treeCache{byRoot: map[string]TreeUsage{}, ttl: TreeTTL}

// MeasureTree returns the size and inode count of a directory tree, cached.
//
// Errors are swallowed per entry rather than aborting: a file that vanishes
// mid-walk is normal in a directory full of running jobs, and returning
// nothing because one entry disappeared would report a machine as using no
// disk at all.
func MeasureTree(root string) TreeUsage {
	shomeTree.mu.Lock()
	defer shomeTree.mu.Unlock()
	if u, ok := shomeTree.byRoot[root]; ok && time.Since(u.At) < shomeTree.ttl {
		return u
	}
	u := TreeUsage{At: time.Now()}
	filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		u.Inodes++
		if d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			// Apparent size, not blocks. A sparse file costs less than this
			// says, but over-reporting is the safe direction for a cap.
			u.Bytes += info.Size()
		}
		return nil
	})
	shomeTree.byRoot[root] = u
	return u
}

// DiskFor reports the filesystem holding path.
func DiskFor(path string) (FSStat, error) {
	// Walk up to something that exists: the root may not be created yet on a
	// first run, and statfs on a missing path tells us nothing.
	for {
		if _, err := os.Stat(path); err == nil {
			break
		}
		parent := filepath.Dir(path)
		if parent == path {
			break
		}
		path = parent
	}
	return statfs(path)
}

// FillDiskAndNet populates the disk, inode and throughput fields of a reading.
//
// Shared by the platform backends because none of it is platform-specific once
// statfs and the interface counters are behind an interface -- and keeping one
// copy means the two cannot report the same machine differently.
func FillDiskAndNet(t *Telemetry, root string, counters NetCounters) {
	if fs, err := DiskFor(root); err == nil && fs.TotalBytes > 0 {
		t.DiskTotalBytes = fs.TotalBytes
		t.DiskFreeBytes = fs.FreeBytes
		t.InodesTotal = fs.TotalInodes
		t.InodesFree = fs.FreeInodes
	}
	if u := MeasureTree(root); u.Inodes > 0 {
		t.ShomeDiskBytes = u.Bytes
		t.ShomeInodes = u.Inodes
	}
	t.NetRxBytesPerSec, t.NetTxBytesPerSec = NetRate(counters)
}
