package linux

import (
	"strings"
	"testing"
)

// A well-equipped bare-metal Linux box.
func full() Environment {
	return Environment{
		Cgroup: CgroupV2, CgroupRoot: "/sys/fs/cgroup", CanDelegate: true,
		IsRoot: true, CanCreateUser: true,
		HasLandlock: true, HasSeccomp: true, HasBwrap: true, BwrapPath: "/usr/bin/bwrap",
	}
}

func TestFullTier(t *testing.T) {
	tier, lost := full().Tier()
	if tier != "full" {
		t.Errorf("tier = %q, want full (lost: %v)", tier, lost)
	}
	// Even a full node discloses the shared-identity limitation, because that
	// is true everywhere until per-user accounts are implemented.
	for _, l := range lost {
		if !containsSubstr([]string{l}, "per-user OS accounts") &&
			!containsSubstr([]string{l}, "could create OS users") {
			t.Errorf("unexpected limitation on a full node: %q", l)
		}
	}
}

func TestCgroupV1DemotesAndExplains(t *testing.T) {
	e := full()
	e.Cgroup = CgroupV1
	tier, lost := e.Tier()
	if tier != "standard" {
		t.Errorf("cgroup v1 should be standard, got %q", tier)
	}
	if !containsSubstr(lost, "cgroup v1") {
		t.Errorf("limitations should name cgroup v1: %v", lost)
	}
}

func TestNoCgroupIsLimited(t *testing.T) {
	e := full()
	e.Cgroup = CgroupAbsent
	e.CanDelegate = false
	tier, lost := e.Tier()
	if tier != "limited" {
		t.Errorf("no cgroups should be limited, got %q", tier)
	}
	if !containsSubstr(lost, "polling") {
		t.Errorf("should say memory falls back to polling: %v", lost)
	}
}

// The constrained case: a real node, honestly degraded.
// Whatever the tier, the node must always disclose that jobs share one
// identity -- that is true on every Linux node today.
func TestAlwaysDisclosesSharedIdentity(t *testing.T) {
	for name, e := range map[string]Environment{"full": full(), "crostini": crostiniEnv()} {
		_, lost := e.Tier()
		if !containsSubstr(lost, "per-user OS accounts are not implemented") {
			t.Errorf("%s: must disclose that jobs share one identity: %v", name, lost)
		}
	}
}

func crostiniEnv() Environment {
	return Environment{
		Cgroup: CgroupV1, CgroupRoot: "/sys/fs/cgroup", CanDelegate: false,
		IsRoot: false, CanCreateUser: false,
		HasLandlock: true, HasSeccomp: true, HasBwrap: true, BwrapPath: "/usr/bin/bwrap",
		Container: "crostini",
	}
}

func TestCrostiniIsLimitedButJoins(t *testing.T) {
	e := Environment{
		Cgroup: CgroupV1, CgroupRoot: "/sys/fs/cgroup", CanDelegate: false,
		IsRoot: false, CanCreateUser: false,
		HasLandlock: false, HasSeccomp: true, HasBwrap: true, BwrapPath: "/usr/bin/bwrap",
		Container: "crostini",
	}
	tier, lost := e.Tier()
	// Crostini can isolate (bubblewrap) but cannot enforce memory or CPU
	// through cgroup v2, which is exactly what "standard" means.
	if tier != "standard" {
		t.Errorf("Crostini should be standard, got %q", tier)
	}
	// The operator has to be told which specific guarantees are missing.
	for _, want := range []string{"cgroup v1", "per-user OS accounts are not implemented", "Crostini"} {
		if !containsSubstr(lost, want) {
			t.Errorf("limitations should mention %q: %v", want, lost)
		}
	}
}

func TestNoIsolationIsLimitedAndLoud(t *testing.T) {
	e := full()
	e.HasBwrap, e.HasLandlock = false, false
	tier, lost := e.Tier()
	if tier != "limited" {
		t.Errorf("no sandbox should be limited, got %q", tier)
	}
	// This is the most serious downgrade there is; it must be unmissable.
	if !containsSubstr(lost, "NO filesystem isolation") {
		t.Errorf("should state plainly that there is no isolation: %v", lost)
	}
}

// Without bubblewrap there is NO filesystem isolation: Landlock is detected
// but not enforced. Claiming otherwise would advertise protection that does
// not exist, which is worse than declining to join.
func TestNoBubblewrapIsLimitedEvenWithLandlock(t *testing.T) {
	e := full()
	e.HasBwrap = false // Landlock still true
	tier, lost := e.Tier()
	if tier != "limited" {
		t.Errorf("tier = %q; without bubblewrap there is no isolation, so it must be limited", tier)
	}
	if !containsSubstr(lost, "NO filesystem isolation") {
		t.Errorf("must state plainly that there is no isolation: %v", lost)
	}
	if !containsSubstr(lost, "does not enforce it yet") {
		t.Errorf("must not imply Landlock substitutes for bubblewrap: %v", lost)
	}
}

func TestMissingLandlockWithBwrapIsStillFull(t *testing.T) {
	e := full()
	e.HasLandlock = false
	tier, lost := e.Tier()
	if tier != "full" {
		t.Errorf("bubblewrap alone still isolates; tier = %q", tier)
	}
	if !containsSubstr(lost, "Landlock unavailable") {
		t.Errorf("should note the missing extra layer: %v", lost)
	}
}

func TestUndelegatedCgroupV2Demotes(t *testing.T) {
	e := full()
	e.CanDelegate = false
	tier, lost := e.Tier()
	if tier != "standard" {
		t.Errorf("undelegated cgroup v2 should be standard, got %q", tier)
	}
	if !containsSubstr(lost, "not delegated") {
		t.Errorf("should explain delegation: %v", lost)
	}
}

func TestSummaryMentionsKeyFacts(t *testing.T) {
	e := full()
	e.NvidiaGPUs = []NvidiaGPU{{Index: 0, Name: "RTX 4090", MemBytes: 24 << 30}}
	s := e.Summary()
	for _, want := range []string{"cgroup=v2", "root=true", "bwrap=true", "cuda=1"} {
		if !strings.Contains(s, want) {
			t.Errorf("summary missing %q: %s", want, s)
		}
	}
}

func containsSubstr(hay []string, needle string) bool {
	for _, h := range hay {
		if strings.Contains(h, needle) {
			return true
		}
	}
	return false
}
