package ctl

import (
	"sync"
	"time"

	"github.com/davidwu/shome/internal/agentapi"
	"github.com/davidwu/shome/internal/platform"
)

// Live cluster metrics: the latest reading from every node, plus enough
// history to draw a trend.
//
// Deliberately in memory and never written to the database. These are samples
// that are worthless within seconds, arriving every few seconds from every
// node; persisting them would turn a job-history database into a time-series
// database, and the failure mode -- a disk filling because a dashboard was
// left open -- is the sort of thing a home cluster should never do to the
// machine it runs on. Losing history on restart is the correct trade.

// HistoryLen is how many samples of per-node history to keep.
//
// At the default heartbeat interval this is roughly the last ten minutes:
// enough for a sparkline to show whether a machine is ramping up or winding
// down, which is the question a trend line actually answers.
const HistoryLen = 120

// Sample is one point of node history, trimmed to what a chart needs.
type Sample struct {
	At     time.Time `json:"at"`
	CPU    float64   `json:"cpu"`
	MemPct float64   `json:"mem"`
	GPU    float64   `json:"gpu"`
	Jobs   int       `json:"jobs"`
}

// nodeMetrics is the live state for one machine.
type nodeMetrics struct {
	latest  platform.Telemetry
	live    []agentapi.JobLive
	history []Sample // ring, oldest first
}

// Metrics collects live telemetry from every node.
type Metrics struct {
	mu    sync.RWMutex
	nodes map[string]*nodeMetrics
}

func NewMetrics() *Metrics { return &Metrics{nodes: map[string]*nodeMetrics{}} }

// Record stores a heartbeat's telemetry.
func (m *Metrics) Record(node string, t platform.Telemetry, live []agentapi.JobLive) {
	if node == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	n := m.nodes[node]
	if n == nil {
		n = &nodeMetrics{}
		m.nodes[node] = n
	}
	n.latest = t
	n.live = live

	s := Sample{At: t.At, CPU: t.CPUPercent, MemPct: t.MemPercent(), GPU: t.GPUPercent, Jobs: len(live)}
	if s.At.IsZero() {
		s.At = time.Now()
	}
	n.history = append(n.history, s)
	if len(n.history) > HistoryLen {
		// Copy rather than reslice: reslicing keeps the whole backing array
		// alive forever, so a long-running controller would hold every sample
		// it ever took while appearing to bound the history.
		n.history = append([]Sample(nil), n.history[len(n.history)-HistoryLen:]...)
	}
}

// Forget drops a node's metrics, for a machine that has left the cluster.
func (m *Metrics) Forget(node string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.nodes, node)
}

// Latest returns the most recent reading for a node.
func (m *Metrics) Latest(node string) (platform.Telemetry, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	n := m.nodes[node]
	if n == nil {
		return platform.Telemetry{}, false
	}
	return n.latest, true
}

// History returns a node's samples, oldest first.
func (m *Metrics) History(node string) []Sample {
	m.mu.RLock()
	defer m.mu.RUnlock()
	n := m.nodes[node]
	if n == nil {
		return nil
	}
	return append([]Sample(nil), n.history...)
}

// LiveJobs returns per-job usage on a node.
func (m *Metrics) LiveJobs(node string) []agentapi.JobLive {
	m.mu.RLock()
	defer m.mu.RUnlock()
	n := m.nodes[node]
	if n == nil {
		return nil
	}
	return append([]agentapi.JobLive(nil), n.live...)
}

// JobUsage finds the live reading for one job, wherever it is running.
func (m *Metrics) JobUsage(id int64) (agentapi.JobLive, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, n := range m.nodes {
		for _, l := range n.live {
			if l.ID == id {
				return l, true
			}
		}
	}
	return agentapi.JobLive{}, false
}

// Stale reports whether a node's newest reading is older than max.
//
// A dashboard that keeps showing the last known numbers for a machine that
// went away half an hour ago is worse than one showing nothing: it reads as a
// healthy node right up until someone wonders why the figures never change.
func (m *Metrics) Stale(node string, max time.Duration, now time.Time) bool {
	t, ok := m.Latest(node)
	if !ok || t.At.IsZero() {
		return true
	}
	return now.Sub(t.At) > max
}
