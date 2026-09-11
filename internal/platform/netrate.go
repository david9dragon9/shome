package platform

import (
	"sync"
	"time"
)

// Network throughput.
//
// Reported, not shaped. Shaping a process's traffic portably is not something
// shome can do without leaving system state behind -- macOS would need pf or
// dnctl rules, Linux tc, both needing root and both surviving an uninstall,
// which conflicts with the promise that removing shome removes shome. So the
// honest offer is measurement: tell the owner what is being moved, and let
// them set a threshold that suspends work rather than one that silently
// throttles it.
//
// Counters are machine-wide, not per-job. Attributing bytes to a process needs
// per-socket accounting that neither platform exposes cheaply, and a number
// labelled "this job's bandwidth" that actually meant "the machine's" would be
// worse than an honest total.

// NetCounters is a cumulative reading of interface traffic.
type NetCounters struct {
	RxBytes uint64
	TxBytes uint64
	At      time.Time
}

// Valid reports whether this is a real reading.
func (n NetCounters) Valid() bool { return !n.At.IsZero() }

// netState remembers the previous counters, since throughput is a rate.
type netState struct {
	mu   sync.Mutex
	last NetCounters
}

var netLast netState

// NetRate turns two cumulative readings into bytes per second.
//
// An unchanged counter is a rate of zero, not an absent reading. That
// distinction matters here in the opposite direction to the CPU rate: a quiet
// network genuinely moved no bytes, and reporting that as "unmeasured" would
// hide a working measurement behind the same dash used for a platform that
// cannot measure at all.
func NetRate(cur NetCounters) (rx, tx float64) {
	netLast.mu.Lock()
	defer netLast.mu.Unlock()
	prev := netLast.last
	if !cur.Valid() {
		return Unknown, Unknown
	}
	netLast.last = cur

	if !prev.Valid() {
		return Unknown, Unknown // nothing to compare against yet
	}
	secs := cur.At.Sub(prev.At).Seconds()
	if secs <= 0 {
		return Unknown, Unknown
	}
	// Counters wrap on 32-bit fields and reset when an interface is
	// recreated. Either shows up as a decrease, which would render as a wild
	// negative spike; treat it as a missing sample and re-baseline instead.
	if cur.RxBytes < prev.RxBytes || cur.TxBytes < prev.TxBytes {
		return Unknown, Unknown
	}
	return float64(cur.RxBytes-prev.RxBytes) / secs,
		float64(cur.TxBytes-prev.TxBytes) / secs
}
