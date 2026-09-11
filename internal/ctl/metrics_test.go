package ctl

import (
	"testing"
	"time"

	"github.com/davidwu/shome/internal/agentapi"
	"github.com/davidwu/shome/internal/platform"
)

func tel(at time.Time, cpu float64, used, total int64) platform.Telemetry {
	t := platform.UnknownTelemetry(at)
	t.CPUPercent, t.MemUsedBytes, t.MemTotalBytes = cpu, used, total
	return t
}

func TestMetricsRecordAndLatest(t *testing.T) {
	m := NewMetrics()
	if _, ok := m.Latest("mini"); ok {
		t.Fatal("an unknown node reported a reading")
	}
	now := time.Now()
	m.Record("mini", tel(now, 42, 4<<30, 8<<30), nil)
	got, ok := m.Latest("mini")
	if !ok {
		t.Fatal("no reading after Record")
	}
	if got.CPUPercent != 42 {
		t.Errorf("CPUPercent = %v, want 42", got.CPUPercent)
	}
	if got.MemPercent() != 50 {
		t.Errorf("MemPercent = %v, want 50", got.MemPercent())
	}
}

// The history is a bounded ring. An unbounded one would grow forever on a
// controller that is meant to run for months on someone's laptop.
func TestHistoryIsBounded(t *testing.T) {
	m := NewMetrics()
	now := time.Now()
	for i := 0; i < HistoryLen*3; i++ {
		m.Record("mini", tel(now.Add(time.Duration(i)*time.Second), float64(i%100), 1, 2), nil)
	}
	h := m.History("mini")
	if len(h) != HistoryLen {
		t.Fatalf("history has %d samples, want %d", len(h), HistoryLen)
	}
	// And it must keep the NEWEST samples, not the first ones it ever saw.
	last := h[len(h)-1]
	want := float64((HistoryLen*3 - 1) % 100)
	if last.CPU != want {
		t.Errorf("newest sample CPU = %v, want %v -- the ring is dropping the wrong end", last.CPU, want)
	}
}

func TestHistoryIsACopy(t *testing.T) {
	m := NewMetrics()
	m.Record("mini", tel(time.Now(), 10, 1, 2), nil)
	h := m.History("mini")
	h[0].CPU = 999
	if again := m.History("mini"); again[0].CPU == 999 {
		t.Fatal("History handed out a reference to internal state")
	}
}

func TestStaleness(t *testing.T) {
	m := NewMetrics()
	now := time.Now()
	m.Record("mini", tel(now, 10, 1, 2), nil)
	if m.Stale("mini", time.Minute, now.Add(30*time.Second)) {
		t.Error("a 30s-old reading was called stale with a 1m window")
	}
	if !m.Stale("mini", time.Minute, now.Add(2*time.Minute)) {
		t.Error("a 2m-old reading was not called stale with a 1m window")
	}
	// A node that never reported is stale, not fresh-and-zero.
	if !m.Stale("unknown", time.Minute, now) {
		t.Error("an unknown node should read as stale")
	}
}

func TestForget(t *testing.T) {
	m := NewMetrics()
	m.Record("mini", tel(time.Now(), 10, 1, 2), nil)
	m.Forget("mini")
	if _, ok := m.Latest("mini"); ok {
		t.Fatal("Forget left the node behind")
	}
}

func TestJobUsageFindsAcrossNodes(t *testing.T) {
	m := NewMetrics()
	now := time.Now()
	m.Record("a", tel(now, 1, 1, 2), []agentapi.JobLive{{ID: 1, MemBytes: 100}})
	m.Record("b", tel(now, 1, 1, 2), []agentapi.JobLive{{ID: 2, MemBytes: 200}})
	got, ok := m.JobUsage(2)
	if !ok || got.MemBytes != 200 {
		t.Fatalf("JobUsage(2) = %+v, %v", got, ok)
	}
	if _, ok := m.JobUsage(99); ok {
		t.Error("found usage for a job that is not running")
	}
}

func TestRecordIgnoresEmptyNodeName(t *testing.T) {
	m := NewMetrics()
	m.Record("", tel(time.Now(), 10, 1, 2), nil)
	if _, ok := m.Latest(""); ok {
		t.Error("recorded telemetry under an empty node name")
	}
}

// Concurrency: heartbeats arrive from every node at once while the dashboard
// reads. A data race here would be a crash in the one component that is
// supposed to tell you the cluster is healthy.
func TestMetricsConcurrentAccess(t *testing.T) {
	m := NewMetrics()
	done := make(chan struct{})
	for i := 0; i < 4; i++ {
		go func(i int) {
			for k := 0; k < 200; k++ {
				m.Record("n", tel(time.Now(), float64(k%100), 1, 2),
					[]agentapi.JobLive{{ID: int64(k)}})
			}
			done <- struct{}{}
		}(i)
	}
	for i := 0; i < 4; i++ {
		go func() {
			for k := 0; k < 200; k++ {
				m.Latest("n")
				m.History("n")
				m.LiveJobs("n")
				m.JobUsage(int64(k))
			}
			done <- struct{}{}
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
}
