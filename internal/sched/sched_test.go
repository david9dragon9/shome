package sched

import (
	"testing"

	"github.com/davidwu/shome/internal/job"
)

func mk(id int64, cpus int, memMiB int64, gpus int) *job.Job {
	return &job.Job{ID: id, State: job.Pending, Spec: job.Spec{
		Limits: job.Limits{CPUs: cpus, MemBytes: memMiB << 20, GPUs: gpus}}}
}

func node(name string, cpus int, memMiB int64, gpus int) NodeState {
	return NodeState{Name: name, CPUs: cpus, MemBytes: memMiB << 20, GPUs: gpus}
}

func TestSchedulesInFIFOOrder(t *testing.T) {
	got := Plan([]*job.Job{mk(1, 1, 100, 0), mk(2, 1, 100, 0)}, []NodeState{node("n1", 4, 1000, 0)})
	for i, d := range got {
		if !d.Scheduled() {
			t.Fatalf("job %d not scheduled: %s", d.JobID, d.Reason)
		}
		if d.JobID != int64(i+1) {
			t.Errorf("out of order: %+v", got)
		}
	}
}

func TestCapacityIsConsumedAcrossDecisions(t *testing.T) {
	// Three 2-CPU jobs on a 4-CPU node: two fit, the third waits.
	got := Plan([]*job.Job{mk(1, 2, 10, 0), mk(2, 2, 10, 0), mk(3, 2, 10, 0)},
		[]NodeState{node("n1", 4, 1000, 0)})
	if !got[0].Scheduled() || !got[1].Scheduled() {
		t.Fatalf("first two should be scheduled: %+v", got)
	}
	if got[2].Scheduled() {
		t.Errorf("third must wait; node has only 4 CPUs: %+v", got[2])
	}
	if got[2].Reason == "" {
		t.Error("a pending job must always carry a reason")
	}
}

func TestStrictFIFODoesNotLetSmallJobsJumpAhead(t *testing.T) {
	// Big job at the head cannot fit; a small job behind it must NOT overtake,
	// or large jobs starve.
	got := Plan([]*job.Job{mk(1, 8, 10, 0), mk(2, 1, 10, 0)}, []NodeState{node("n1", 4, 1000, 0)})
	if got[0].Scheduled() {
		t.Fatal("8-CPU job should not fit on a 4-CPU node")
	}
	if got[1].Scheduled() {
		t.Error("small job overtook a blocked head-of-queue job; that starves large jobs")
	}
	if got[1].Reason != "queued behind an earlier job" {
		t.Errorf("unhelpful reason: %q", got[1].Reason)
	}
}

func TestImpossibleRequestExplainsItself(t *testing.T) {
	got := Plan([]*job.Job{mk(1, 99, 10, 0)}, []NodeState{node("n1", 4, 1000, 0)})
	if got[0].Scheduled() {
		t.Fatal("should not schedule")
	}
	// Must distinguish "will never fit" from "wait your turn".
	want := "requests 99 CPUs, node has 4"
	if got[0].Reason != want {
		t.Errorf("reason = %q, want %q", got[0].Reason, want)
	}
}

func TestDrainedNodeAcceptsNothing(t *testing.T) {
	n := node("n1", 8, 1000, 0)
	n.Drained = true
	got := Plan([]*job.Job{mk(1, 1, 10, 0)}, []NodeState{n})
	if got[0].Scheduled() {
		t.Error("drained node must not accept jobs; the owner's pause is absolute")
	}
	if got[0].Reason != "node is drained by its owner" {
		t.Errorf("reason = %q", got[0].Reason)
	}
}

func TestGPUExclusivity(t *testing.T) {
	// Apple Silicon has no MPS/MIG equivalent, so a GPU is allocated whole.
	got := Plan([]*job.Job{mk(1, 1, 10, 1), mk(2, 1, 10, 1)}, []NodeState{node("n1", 8, 1000, 1)})
	if !got[0].Scheduled() {
		t.Fatal("first GPU job should run")
	}
	if got[1].Scheduled() {
		t.Error("second GPU job must wait: the GPU is exclusive")
	}
}

func TestPlanDoesNotMutateInput(t *testing.T) {
	nodes := []NodeState{node("n1", 4, 1000, 0)}
	Plan([]*job.Job{mk(1, 2, 10, 0)}, nodes)
	if nodes[0].UsedCPUs != 0 {
		t.Error("Plan mutated the caller's node slice; speculative planning would corrupt state")
	}
}

func TestSpreadsAcrossNodes(t *testing.T) {
	got := Plan([]*job.Job{mk(1, 4, 10, 0), mk(2, 4, 10, 0)},
		[]NodeState{node("n1", 4, 1000, 0), node("n2", 4, 1000, 0)})
	if !got[0].Scheduled() || !got[1].Scheduled() {
		t.Fatalf("both should fit across two nodes: %+v", got)
	}
	if got[0].Node == got[1].Node {
		t.Errorf("both landed on %s; capacity accounting is wrong", got[0].Node)
	}
}
