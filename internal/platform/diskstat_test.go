package platform

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMeasureTree(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "a", "b"), 0o700)
	os.WriteFile(filepath.Join(root, "one.txt"), make([]byte, 1000), 0o600)
	os.WriteFile(filepath.Join(root, "a", "two.txt"), make([]byte, 2000), 0o600)

	u := MeasureTree(root)
	if u.Bytes != 3000 {
		t.Errorf("Bytes = %d, want 3000", u.Bytes)
	}
	// Directories count: they are inodes, which is what the kernel runs out of.
	if u.Inodes < 5 {
		t.Errorf("Inodes = %d, want at least 5 (root, a, a/b, two files)", u.Inodes)
	}
}

// The cache is keyed by path. A single shared entry was the first attempt and
// handed two backends measuring different roots each other's figure -- which
// reads as a plausible number rather than as a bug.
func TestMeasureTreeCacheIsPerPath(t *testing.T) {
	small := t.TempDir()
	big := t.TempDir()
	os.WriteFile(filepath.Join(small, "f"), make([]byte, 10), 0o600)
	os.WriteFile(filepath.Join(big, "f"), make([]byte, 100_000), 0o600)

	a := MeasureTree(small)
	b := MeasureTree(big)
	if a.Bytes == b.Bytes {
		t.Fatalf("both roots measured %d bytes; the cache is not keyed by path", a.Bytes)
	}
	if a.Bytes != 10 || b.Bytes != 100_000 {
		t.Errorf("small=%d big=%d, want 10 and 100000", a.Bytes, b.Bytes)
	}
}

func TestMeasureTreeMissingRoot(t *testing.T) {
	u := MeasureTree(filepath.Join(t.TempDir(), "never-created"))
	if u.Bytes != 0 || u.Inodes != 0 {
		t.Errorf("a missing root measured %+v", u)
	}
}

func TestDiskForReportsSomething(t *testing.T) {
	fs, err := DiskFor(t.TempDir())
	if err != nil {
		t.Skipf("statfs unavailable: %v", err)
	}
	if fs.TotalBytes <= 0 {
		t.Errorf("TotalBytes = %d", fs.TotalBytes)
	}
	if fs.FreeBytes < 0 || fs.FreeBytes > fs.TotalBytes {
		t.Errorf("FreeBytes = %d, outside 0..%d", fs.FreeBytes, fs.TotalBytes)
	}
	if fs.UsedBytes() < 0 {
		t.Errorf("UsedBytes = %d", fs.UsedBytes())
	}
}

// A path that does not exist yet is normal on a first run, and must still
// report the filesystem it would live on.
func TestDiskForWalksUpToAnExistingPath(t *testing.T) {
	deep := filepath.Join(t.TempDir(), "not", "yet", "created")
	fs, err := DiskFor(deep)
	if err != nil {
		t.Fatalf("a not-yet-created path should resolve to its parent volume: %v", err)
	}
	if fs.TotalBytes <= 0 {
		t.Errorf("TotalBytes = %d", fs.TotalBytes)
	}
}

// An idle network is a rate of zero, not an absent reading. Reporting it as
// unmeasured would hide a working measurement behind the dash used for a
// platform that cannot measure at all.
func TestNetRateIdleIsZeroNotUnknown(t *testing.T) {
	netLast.mu.Lock()
	netLast.last = NetCounters{}
	netLast.mu.Unlock()

	now := time.Now()
	if rx, _ := NetRate(NetCounters{RxBytes: 1000, TxBytes: 500, At: now}); rx != Unknown {
		t.Errorf("first reading = %v, want Unknown", rx)
	}
	rx, tx := NetRate(NetCounters{RxBytes: 1000, TxBytes: 500, At: now.Add(time.Second)})
	if rx != 0 || tx != 0 {
		t.Errorf("unchanged counters gave %v/%v, want 0/0", rx, tx)
	}
	rx, tx = NetRate(NetCounters{RxBytes: 3000, TxBytes: 1500, At: now.Add(2 * time.Second)})
	if rx != 2000 || tx != 1000 {
		t.Errorf("got %v/%v, want 2000/1000 bytes per second", rx, tx)
	}
}

// A counter that goes backwards means a wrap or a recreated interface. It must
// not render as a wild negative spike.
func TestNetRateHandlesCounterReset(t *testing.T) {
	netLast.mu.Lock()
	netLast.last = NetCounters{}
	netLast.mu.Unlock()

	now := time.Now()
	NetRate(NetCounters{RxBytes: 1_000_000, TxBytes: 500_000, At: now})
	rx, tx := NetRate(NetCounters{RxBytes: 10, TxBytes: 5, At: now.Add(time.Second)})
	if rx != Unknown || tx != Unknown {
		t.Errorf("a counter reset gave %v/%v, want Unknown", rx, tx)
	}
	// And it re-baselines rather than staying stuck.
	rx, _ = NetRate(NetCounters{RxBytes: 1010, TxBytes: 5, At: now.Add(2 * time.Second)})
	if rx != 1000 {
		t.Errorf("after a reset got %v, want 1000", rx)
	}
}
