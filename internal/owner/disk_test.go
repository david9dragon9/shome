package owner

import (
	"strings"
	"testing"
)

func state(shomeBytes, shomeInodes, freeBytes, freeInodes int64) DiskState {
	return DiskState{ShomeBytes: shomeBytes, ShomeInodes: shomeInodes,
		FreeBytes: freeBytes, FreeInodes: freeInodes, Known: true}
}

const gb = int64(1) << 30

// The bug this replaces: max_disk_gb was parsed and read by nothing, so an
// owner who set it believed in a limit that did not exist. A setting that
// looks enforced and is not is worse than no setting at all.
func TestDiskCapIsEnforced(t *testing.T) {
	p := &Policy{Contribute: Contribute{MaxDiskGB: 10}}
	if why := p.CheckDisk(state(5*gb, 100, 500*gb, 1e6)); why != "" {
		t.Errorf("work refused while inside the cap: %s", why)
	}
	why := p.CheckDisk(state(11*gb, 100, 500*gb, 1e6))
	if why == "" {
		t.Fatal("work accepted past the disk cap")
	}
	// The message must carry both numbers, or an owner cannot tell how far
	// over they are.
	if !strings.Contains(why, "11.0 GiB") || !strings.Contains(why, "10.0 GiB") {
		t.Errorf("unhelpful message: %s", why)
	}
}

// Inodes run out independently of space: a million empty files occupy almost
// nothing and can still make a filesystem unusable.
func TestInodeCapIsEnforced(t *testing.T) {
	p := &Policy{Contribute: Contribute{MaxInodes: 1000}}
	if why := p.CheckDisk(state(1*gb, 500, 500*gb, 1e6)); why != "" {
		t.Errorf("refused while inside the inode cap: %s", why)
	}
	if why := p.CheckDisk(state(1*gb, 1500, 500*gb, 1e6)); why == "" {
		t.Error("accepted past the inode cap")
	}
	// A byte cap says nothing about inodes, and must not be read as if it did.
	byteOnly := &Policy{Contribute: Contribute{MaxDiskGB: 100}}
	if why := byteOnly.CheckDisk(state(1*gb, 50_000_000, 500*gb, 1e6)); why != "" {
		t.Errorf("a byte cap refused work over inodes: %s", why)
	}
}

// A contribution cap alone does not protect the machine: shome can be well
// inside its allowance while something else has taken the disk to nothing.
func TestFreeSpaceFloorIsEnforced(t *testing.T) {
	p := &Policy{Contribute: Contribute{MaxDiskGB: 500, MinFreeDiskGB: 20}}
	// Inside its own cap, but the volume is nearly full.
	why := p.CheckDisk(state(1*gb, 100, 5*gb, 1e6))
	if why == "" {
		t.Fatal("accepted work with the volume nearly full")
	}
	if !strings.Contains(why, "keep") {
		t.Errorf("the message does not explain whose rule this is: %s", why)
	}
	if why := p.CheckDisk(state(1*gb, 100, 100*gb, 1e6)); why != "" {
		t.Errorf("refused with plenty free: %s", why)
	}
}

// A filesystem out of inodes cannot create a scratch directory, so there is
// nothing useful to accept even if the owner said nothing.
func TestExhaustedInodesRefuseWorkWithoutAPolicy(t *testing.T) {
	p := &Policy{}
	if why := p.CheckDisk(state(1*gb, 10, 500*gb, 50)); why == "" {
		t.Error("accepted work on a filesystem with 50 inodes left")
	}
}

// A limit that cannot be measured must not be enforced by guessing.
func TestUnmeasuredDiskNeverRefuses(t *testing.T) {
	p := &Policy{Contribute: Contribute{MaxDiskGB: 1, MinFreeDiskGB: 100}}
	if why := p.CheckDisk(DiskState{Known: false}); why != "" {
		t.Errorf("refused work on an unmeasurable platform: %s", why)
	}
}

// No policy means no disk rule, not a default one.
func TestNoPolicyNoDiskRule(t *testing.T) {
	var p *Policy
	if why := p.CheckDisk(state(900*gb, 1e9, 1*gb, 10)); why != "" {
		// The exhausted-inode backstop only applies once a policy exists;
		// with none at all the owner has said nothing and shome should not
		// invent a rule.
		if !strings.Contains(why, "inodes left") {
			t.Errorf("a nil policy produced a rule: %s", why)
		}
	}
}

func TestDescribeDiskExplainsThePosition(t *testing.T) {
	p := &Policy{Contribute: Contribute{MaxDiskGB: 200, MinFreeDiskGB: 20}}
	got := p.DescribeDisk(state(50*gb, 1000, 300*gb, 1e6))
	for _, want := range []string{"50.0 GiB", "200.0 GiB", "300.0 GiB", "20.0 GiB"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q missing from: %s", want, got)
		}
	}
	if got := p.DescribeDisk(DiskState{}); !strings.Contains(got, "not measured") {
		t.Errorf("an unmeasured platform should say so: %s", got)
	}
}
