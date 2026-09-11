package ctl

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/davidwu/shome/internal/agentapi"
	"github.com/davidwu/shome/internal/job"
	"github.com/davidwu/shome/internal/platform"
	"github.com/davidwu/shome/internal/store"
)

// reclaimFixture is a controller with one node and jobs recorded on it.
func reclaimFixture(t *testing.T) (*Controller, *store.Store, context.Context, time.Time) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	c := New(st, t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	now := time.Now()
	st.UpsertNode(ctx, "worker", "10.0.0.9", platform.Capabilities{OS: "linux", CPUs: 4}, now)
	return c, st, ctx, now
}

// A job the node no longer has is resolved from the node's own report, and
// only then.
//
// The controller used to fail every RUNNING job when it started, on the
// theory that it must have crashed. That cannot tell a crash from a planned
// restart, and an agent keeps supervising its jobs when it loses the
// controller -- so `shome restart` failed live work on every machine in the
// cluster, and overrode the deliberate choice to wait out an unreachable
// node rather than give up on its jobs.
func TestAJobIsOnlyLostWhenItsNodeSaysSo(t *testing.T) {
	c, st, ctx, now := reclaimFixture(t)
	mk := func(name string, requeue bool) int64 {
		j, err := st.Submit(ctx, job.Spec{
			Name: name, User: "u", Script: "/bin/true", ArrayTaskID: -1, Requeue: requeue,
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		// Started well before the claim grace, so the node is expected to
		// know about it by now.
		if err := st.MarkRunning(ctx, j.ID, "worker", "", now.Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
		return j.ID
	}
	kept, plain, opted := mk("kept", false), mk("plain", false), mk("opted", true)

	// An agent that cannot say what it has changes nothing. This is the
	// version-skew case: an empty list from an agent that does not report
	// claims must not read as "running nothing".
	//
	// It must also not consume the rate limit, or the pass that matters
	// below would be skipped -- which is why the check comes after it.
	c.reclaimUnclaimed(ctx, agentapi.Heartbeat{Node: "worker"}, time.Now())
	for _, id := range []int64{kept, plain, opted} {
		if j, _ := st.Get(ctx, id); j.State != job.Running {
			t.Fatalf("job %d = %s after a heartbeat that said nothing; want RUNNING",
				id, j.State)
		}
	}

	// A complete report naming only one of them: the other two are gone, and
	// what happens to each depends on --requeue.
	c.reclaimUnclaimed(ctx, agentapi.Heartbeat{
		Node: "worker", Claimed: []int64{kept}, ClaimsOK: true,
	}, time.Now())

	if j, _ := st.Get(ctx, kept); j.State != job.Running {
		t.Errorf("the claimed job = %s, want RUNNING", j.State)
	}
	j, _ := st.Get(ctx, plain)
	if j.State != job.Failed {
		t.Errorf("an unclaimed job = %s, want FAILED", j.State)
	}
	if !strings.Contains(j.Reason, "no longer has this job") {
		t.Errorf("reason = %q; it should say the node does not have it", j.Reason)
	}
	if !strings.Contains(j.Reason, "--requeue") {
		t.Errorf("reason = %q; it should say how to have it re-run", j.Reason)
	}
	if o, _ := st.Get(ctx, opted); o.State != job.Pending {
		t.Errorf("a --requeue job = %s, want PENDING", o.State)
	}
}

// A result arriving in the same heartbeat is not a lost job.
func TestAJobReportingItsResultIsNotLost(t *testing.T) {
	c, st, ctx, now := reclaimFixture(t)
	j, err := st.Submit(ctx, job.Spec{
		Name: "finishing", User: "u", Script: "/bin/true", ArrayTaskID: -1,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	st.MarkRunning(ctx, j.ID, "worker", "", now.Add(-time.Hour))
	// The agent has dropped it from its running set and is reporting the
	// outcome; the outcome is recorded elsewhere in the same heartbeat.
	c.reclaimUnclaimed(ctx, agentapi.Heartbeat{
		Node: "worker", ClaimsOK: true,
		Jobs: []agentapi.JobStatus{{ID: j.ID, State: job.Completed}},
	}, time.Now())
	if got, _ := st.Get(ctx, j.ID); got.State == job.Failed {
		t.Errorf("a job whose result was in the same heartbeat was failed: %q", got.Reason)
	}
}

// A job placed moments ago is not lost merely because the node has not
// picked it up yet.
func TestAFreshlyPlacedJobIsNotLost(t *testing.T) {
	c, st, ctx, now := reclaimFixture(t)
	j, err := st.Submit(ctx, job.Spec{
		Name: "fresh", User: "u", Script: "/bin/true", ArrayTaskID: -1,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	// A job is marked RUNNING when it is assigned, which is before the agent
	// has seen the action.
	if err := st.MarkRunning(ctx, j.ID, "worker", "", now); err != nil {
		t.Fatal(err)
	}
	c.reclaimUnclaimed(ctx, agentapi.Heartbeat{Node: "worker", ClaimsOK: true}, now)
	if got, _ := st.Get(ctx, j.ID); got.State != job.Running {
		t.Errorf("state = %s; a job inside the claim grace must be left alone", got.State)
	}
}
