package platform

import "time"

// CPURate turns two cumulative CPU tick counters into a utilisation
// percentage. Shared by the darwin and linux backends, which read the same
// shape of data from different places.
//
// It lives here, separate from either backend, because it is the part with
// actual logic in it -- everything around it is a syscall or a file read. A
// rule that depends on the host's timing cannot be tested reliably against
// the host; this can be tested exactly.

// CPUTicks is one cumulative reading. Busy excludes idle and I/O wait; Total
// includes them, so it advances at a fixed rate whatever the machine is doing.
type CPUTicks struct {
	Busy, Total uint64
	At          time.Time
}

// Valid reports whether this is a real reading rather than a zero value.
func (c CPUTicks) Valid() bool { return !c.At.IsZero() }

// CPURate computes utilisation between two readings and returns the baseline
// to keep for next time.
//
// The baseline only advances when the counters actually moved. Kernel tick
// counters update coarsely -- tens of milliseconds -- so a caller polling
// faster than that granularity sees two identical readings. Replacing the
// baseline anyway means every future comparison is also against an unmoved
// counter, and the caller never gets a number at all. Holding it until it
// moves makes the reading correct at any polling rate; the only cost is that
// it averages over a slightly longer window than the caller asked for.
func CPURate(prev, cur CPUTicks) (percent float64, baseline CPUTicks) {
	moved := cur.Total > prev.Total
	baseline = prev
	if moved || !prev.Valid() {
		baseline = cur
	}
	if !prev.Valid() || !moved {
		return Unknown, baseline
	}
	dTotal := cur.Total - prev.Total
	dBusy := cur.Busy - prev.Busy
	// Busy can appear to go backwards across a counter reset or a CPU going
	// offline. Reporting a negative percentage would render as a broken bar,
	// so treat it as unknown and let the next sample re-establish a baseline.
	if cur.Busy < prev.Busy {
		return Unknown, cur
	}
	return clamp100(100 * float64(dBusy) / float64(dTotal)), baseline
}

func clamp100(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}
