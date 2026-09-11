package platform

import (
	"testing"
	"time"
)

func ticks(busy, total uint64, at time.Time) CPUTicks {
	return CPUTicks{Busy: busy, Total: total, At: at}
}

func TestCPURateNeedsTwoSamples(t *testing.T) {
	now := time.Now()
	// Nothing to compare against: the answer is unknown, not zero. An idle
	// reading here would be indistinguishable from a genuinely idle machine.
	pct, base := CPURate(CPUTicks{}, ticks(100, 200, now))
	if pct != Unknown {
		t.Errorf("first sample gave %v, want Unknown", pct)
	}
	if !base.Valid() || base.Total != 200 {
		t.Errorf("first sample did not become the baseline: %+v", base)
	}
}

func TestCPURateComputesPercentage(t *testing.T) {
	now := time.Now()
	prev := ticks(1000, 2000, now)
	cur := ticks(1075, 2100, now.Add(time.Second)) // 75 busy of 100 total
	pct, base := CPURate(prev, cur)
	if pct != 75 {
		t.Errorf("got %v%%, want 75%%", pct)
	}
	if base.Total != cur.Total {
		t.Errorf("baseline did not advance after a real delta")
	}
}

// The bug this function exists to fix: sampling faster than the kernel's tick
// granularity produced two identical readings, and replacing the baseline each
// time meant every subsequent comparison was also against an unmoved counter,
// so the caller never got a number at all.
func TestCPURateHoldsBaselineWhenCountersStall(t *testing.T) {
	now := time.Now()
	base := ticks(1000, 2000, now)

	// Several samples arrive before the counters move.
	stalled := ticks(1000, 2000, now.Add(time.Millisecond))
	for i := 0; i < 5; i++ {
		var pct float64
		pct, base = CPURate(base, stalled)
		if pct != Unknown {
			t.Fatalf("stalled sample %d reported %v, want Unknown", i, pct)
		}
		if base.Total != 2000 || base.Busy != 1000 {
			t.Fatalf("baseline moved to %+v during a stall; it must be held", base)
		}
	}
	// When they finally move, the reading is against the original baseline.
	moved := ticks(1050, 2100, now.Add(100*time.Millisecond))
	pct, base := CPURate(base, moved)
	if pct != 50 {
		t.Fatalf("after the stall got %v%%, want 50%% measured from the held baseline", pct)
	}
	if base.Total != 2100 {
		t.Errorf("baseline did not advance once the counters moved")
	}
}

// A counter reset (or a core going offline) can make busy appear to fall.
// Reporting a negative percentage would draw as a broken bar.
func TestCPURateHandlesGoingBackwards(t *testing.T) {
	now := time.Now()
	prev := ticks(5000, 9000, now)
	cur := ticks(100, 9500, now.Add(time.Second))
	pct, base := CPURate(prev, cur)
	if pct != Unknown {
		t.Errorf("got %v after a counter reset, want Unknown", pct)
	}
	if base.Busy != 100 {
		t.Errorf("baseline should re-establish from the new reading, got %+v", base)
	}
}

func TestCPURateClampsToRange(t *testing.T) {
	now := time.Now()
	// Busy rising faster than total should not exceed 100%.
	pct, _ := CPURate(ticks(0, 0, now), ticks(500, 100, now.Add(time.Second)))
	if pct < 0 || pct > 100 {
		t.Errorf("got %v, outside 0..100", pct)
	}
}

func TestCPURateIsIdleAtZeroBusy(t *testing.T) {
	now := time.Now()
	pct, _ := CPURate(ticks(1000, 2000, now), ticks(1000, 2100, now.Add(time.Second)))
	if pct != 0 {
		t.Errorf("an idle interval reported %v%%, want 0%%", pct)
	}
}
