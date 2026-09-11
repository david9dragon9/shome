package sched

import (
	"testing"
	"time"

	"github.com/davidwu/shome/internal/job"
)

var t0 = time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

func bfJob(id int64, cpus int, gpus int, wall time.Duration) *job.Job {
	return &job.Job{ID: id, Spec: job.Spec{
		User: "u", Limits: job.Limits{CPUs: cpus, GPUs: gpus, Walltime: wall},
	}}
}

func bfNode(name string, cpus, gpus int, running ...RunningJob) NodeState {
	n := NodeState{Name: name, CPUs: cpus, GPUs: gpus, MemBytes: 64 << 30,
		Online: true, Running: running}
	for _, r := range running {
		c := r.CPUs
		if c <= 0 {
			c = 1
		}
		n.UsedCPUs += c
		n.UsedGPUs += r.GPUs
		n.UsedMem += r.MemBytes
	}
	return n
}

func decide(ds []Decision, id int64) Decision {
	for _, d := range ds {
		if d.JobID == id {
			return d
		}
	}
	return Decision{}
}

// The problem backfill exists to solve: without it, a big job at the head of
// the queue stalls small jobs on idle machines.
func TestWithoutBackfillABigJobStallsTheQueue(t *testing.T) {
	nodes := []NodeState{bfNode("a", 4, 0)}
	pending := []*job.Job{
		bfJob(1, 8, 0, time.Hour), // cannot ever fit on a 4-CPU node
		bfJob(2, 1, 0, time.Minute),
	}
	ds := Plan(pending, nodes)
	if decide(ds, 2).Scheduled() {
		t.Error("strict FIFO let a later job past a blocked one")
	}
	if decide(ds, 2).Reason == "" {
		t.Error("the blocked job was given no reason")
	}
}

// With backfill on, the small job runs -- and the reason the big one is
// waiting still says something useful.
func TestBackfillRunsSmallJobsPastABlockedOne(t *testing.T) {
	nodes := []NodeState{bfNode("a", 4, 0)}
	pending := []*job.Job{
		bfJob(1, 8, 0, time.Hour),
		bfJob(2, 1, 0, time.Minute),
		bfJob(3, 1, 0, time.Minute),
	}
	ds := PlanWith(pending, nodes, Options{Backfill: true, Now: t0})
	if decide(ds, 1).Scheduled() {
		t.Error("a job too large for any node was scheduled")
	}
	for _, id := range []int64{2, 3} {
		if !decide(ds, id).Scheduled() {
			t.Errorf("job %d was not backfilled: %q", id, decide(ds, id).Reason)
		}
	}
}

// The safety property: a job that cannot be shown to finish before the
// reservation must not take the reserved capacity, or the big job starves.
func TestBackfillHoldsTheGapForTheBlockedJob(t *testing.T) {
	// One node, 4 CPUs, all busy with a job ending in 10 minutes.
	busy := RunningJob{ID: 99, CPUs: 4, EndsAt: t0.Add(10 * time.Minute)}
	nodes := []NodeState{bfNode("a", 4, 0, busy)}

	pending := []*job.Job{
		bfJob(1, 4, 0, time.Hour), // needs the whole node, waits for 99
		bfJob(2, 4, 0, time.Hour), // would still be running at the handover
		bfJob(3, 4, 0, 0),         // no time limit at all
	}
	ds := PlanWith(pending, nodes, Options{Backfill: true, Now: t0})

	if decide(ds, 1).Scheduled() {
		t.Fatal("job 1 was scheduled onto a full node")
	}
	// A reservation should have been worked out and explained.
	if r := decide(ds, 1).Reason; r == "" || !containsAll(r, "holding", "a") {
		t.Errorf("job 1's reason does not mention the reservation: %q", r)
	}
	for _, id := range []int64{2, 3} {
		if decide(ds, id).Scheduled() {
			t.Errorf("job %d took capacity reserved for job 1", id)
		}
	}
}

// A job that *will* finish before the handover is exactly what backfill is
// for, and must be allowed through.
func TestBackfillAllowsAJobThatFinishesInTime(t *testing.T) {
	// 4 CPUs, 2 in use until 30 minutes from now.
	busy := RunningJob{ID: 99, CPUs: 2, EndsAt: t0.Add(30 * time.Minute)}
	nodes := []NodeState{bfNode("a", 4, 0, busy)}

	pending := []*job.Job{
		bfJob(1, 4, 0, time.Hour),     // needs all 4: waits for 99 to end
		bfJob(3, 2, 0, 2*time.Hour),   // fits now, but would overrun the handover
		bfJob(2, 2, 0, 5*time.Minute), // fits now, done long before it
	}
	ds := PlanWith(pending, nodes, Options{Backfill: true, Now: t0})

	if !decide(ds, 2).Scheduled() {
		t.Errorf("a job that finishes before the handover was refused: %q",
			decide(ds, 2).Reason)
	}
	if decide(ds, 3).Scheduled() {
		t.Error("a job that would overrun the reservation was allowed in")
	}
	if r := decide(ds, 3).Reason; !containsAll(r, "held for a higher-priority job") {
		t.Errorf("job 3's reason does not explain the reservation: %q", r)
	}
}

// A job with no --time cannot be shown to finish, so it never backfills into
// a reserved gap -- however small it is.
func TestUnboundedJobNeverBackfillsIntoAReservation(t *testing.T) {
	busy := RunningJob{ID: 99, CPUs: 2, EndsAt: t0.Add(30 * time.Minute)}
	nodes := []NodeState{bfNode("a", 4, 0, busy)}
	pending := []*job.Job{
		bfJob(1, 4, 0, time.Hour),
		bfJob(2, 1, 0, 0), // tiny, but unbounded
	}
	ds := PlanWith(pending, nodes, Options{Backfill: true, Now: t0})
	if decide(ds, 2).Scheduled() {
		t.Error("an unbounded job slipped into a reserved gap; it could hold " +
			"it indefinitely, which is the starvation the reservation prevents")
	}
	if r := decide(ds, 2).Reason; !containsAll(r, "no --time limit") {
		t.Errorf("the reason does not tell the user how to fix it: %q", r)
	}
}

// Only the reserved node is held. Other machines stay available, or one
// blocked job would idle the whole cluster.
func TestReservationHoldsOneNodeNotTheCluster(t *testing.T) {
	busy := RunningJob{ID: 99, CPUs: 4, EndsAt: t0.Add(10 * time.Minute)}
	nodes := []NodeState{bfNode("a", 4, 0, busy), bfNode("b", 4, 0)}

	pending := []*job.Job{
		bfJob(1, 4, 0, time.Hour), // wants a whole node
		bfJob(2, 4, 0, time.Hour), // should take the free node b
	}
	ds := PlanWith(pending, nodes, Options{Backfill: true, Now: t0})
	// Job 1 fits on b right now, so it should simply run there.
	if !decide(ds, 1).Scheduled() {
		t.Fatalf("job 1 did not take the free node: %q", decide(ds, 1).Reason)
	}
	if decide(ds, 1).Node != "b" {
		t.Errorf("job 1 landed on %q, want the free node b", decide(ds, 1).Node)
	}
}

// When nothing running has a time limit, no honest reservation can be made.
// The queue must keep moving rather than stalling on an unpredictable job.
func TestNoPredictionWhenRunningJobsAreUnbounded(t *testing.T) {
	// 3 of 4 CPUs held by a job with no time limit, so its end cannot be
	// predicted -- but one CPU is genuinely free.
	busy := RunningJob{ID: 99, CPUs: 3} // no EndsAt
	nodes := []NodeState{bfNode("a", 4, 0, busy)}
	pending := []*job.Job{
		bfJob(1, 4, 0, time.Hour),
		bfJob(2, 1, 0, time.Minute),
	}
	ds := PlanWith(pending, nodes, Options{Backfill: true, Now: t0})
	if decide(ds, 1).Scheduled() {
		t.Fatal("job 1 was scheduled onto a full node")
	}
	if r := decide(ds, 1).Reason; !containsAll(r, "cannot predict") {
		t.Errorf("reason should admit it cannot predict: %q", r)
	}
	// The queue keeps moving; job 2 does not have to wait on a job whose
	// start cannot be estimated.
	if !decide(ds, 2).Scheduled() {
		t.Errorf("the queue stalled on an unpredictable job: %q", decide(ds, 2).Reason)
	}
}

// Depth bounds how many blocked jobs get a reservation, which bounds how much
// of the cluster is held idle waiting for them.
//
// Observed with a job too long to backfill into any gap: with one reservation
// it can still use the un-held machine, and with two it has nowhere to go.
func TestBackfillDepthBoundsReservations(t *testing.T) {
	// Each node has 3 of 4 CPUs busy, so a 4-CPU job must wait on either and
	// a 1-CPU job has somewhere to go.
	b1 := RunningJob{ID: 91, CPUs: 3, EndsAt: t0.Add(10 * time.Minute)}
	b2 := RunningJob{ID: 92, CPUs: 3, EndsAt: t0.Add(20 * time.Minute)}
	nodes := []NodeState{bfNode("a", 4, 0, b1), bfNode("b", 4, 0, b2)}

	pending := []*job.Job{
		bfJob(1, 4, 0, time.Hour),
		bfJob(2, 4, 0, time.Hour),
		// Two hours: longer than either reservation, so it cannot be shown
		// to finish before a handover and may only use an un-held machine.
		bfJob(3, 1, 0, 2*time.Hour),
	}

	one := PlanWith(pending, nodes, Options{Backfill: true, Depth: 1, Now: t0})
	if !decide(one, 3).Scheduled() {
		t.Errorf("with one reservation the long job should have used the "+
			"un-held machine: %q", decide(one, 3).Reason)
	}
	if got := decide(one, 3).Node; got != "b" {
		t.Errorf("the long job took %q; node a is held for job 1", got)
	}

	two := PlanWith(pending, nodes, Options{Backfill: true, Depth: 2, Now: t0})
	if decide(two, 3).Scheduled() {
		t.Error("with both machines reserved the long job had nowhere to go, " +
			"but ran anyway")
	}
}

// A short job may use reserved capacity, because it demonstrably hands it
// back before the reservation comes due. That is the whole point.
func TestShortJobUsesAReservedGap(t *testing.T) {
	b1 := RunningJob{ID: 91, CPUs: 3, EndsAt: t0.Add(10 * time.Minute)}
	nodes := []NodeState{bfNode("a", 4, 0, b1)}
	pending := []*job.Job{
		bfJob(1, 4, 0, time.Hour),   // reserves node a from t0+10m
		bfJob(2, 1, 0, time.Minute), // done at t0+1m, long before that
	}
	ds := PlanWith(pending, nodes, Options{Backfill: true, Now: t0})
	if !decide(ds, 2).Scheduled() {
		t.Errorf("a one-minute job could not use a ten-minute gap: %q",
			decide(ds, 2).Reason)
	}
}

// Backfill must never overcommit a node: two jobs cannot both take the same
// free capacity in one pass.
func TestBackfillDoesNotOvercommit(t *testing.T) {
	nodes := []NodeState{bfNode("a", 4, 0)}
	pending := []*job.Job{
		bfJob(1, 16, 0, time.Hour), // impossible
		bfJob(2, 3, 0, time.Minute),
		bfJob(3, 3, 0, time.Minute), // only 1 CPU left after job 2
	}
	ds := PlanWith(pending, nodes, Options{Backfill: true, Now: t0})
	n := 0
	for _, id := range []int64{2, 3} {
		if decide(ds, id).Scheduled() {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d of two 3-CPU jobs ran on a 4-CPU node", n)
	}
}

// The zero Options must behave exactly as Plan always did.
func TestZeroOptionsIsStrictFIFO(t *testing.T) {
	nodes := []NodeState{bfNode("a", 4, 0)}
	pending := []*job.Job{bfJob(1, 8, 0, time.Hour), bfJob(2, 1, 0, time.Minute)}
	a := PlanWith(pending, nodes, Options{})
	b := Plan(pending, nodes)
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("PlanWith(zero) differs from Plan: %+v vs %+v", a[i], b[i])
		}
	}
}

// Backfill with no clock cannot reason about time, so it must only use
// capacity no reservation wants -- never guess.
func TestBackfillWithoutAClockIsConservative(t *testing.T) {
	busy := RunningJob{ID: 99, CPUs: 2, EndsAt: t0.Add(time.Minute)}
	nodes := []NodeState{bfNode("a", 4, 0, busy)}
	pending := []*job.Job{
		bfJob(1, 4, 0, time.Hour),
		bfJob(2, 1, 0, time.Second),
	}
	ds := PlanWith(pending, nodes, Options{Backfill: true}) // no Now
	if decide(ds, 1).Scheduled() {
		t.Error("job 1 was scheduled onto a full node")
	}
	// With no reservation possible, the queue still moves.
	if !decide(ds, 2).Scheduled() {
		t.Errorf("job 2 was refused with no reservation in force: %q",
			decide(ds, 2).Reason)
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		found := false
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
