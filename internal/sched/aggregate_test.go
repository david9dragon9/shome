package sched

import (
	"strings"
	"testing"
)

func n(name string, cpus int, memGiB int64, gpus int, gpuMemGiB int64) NodeState {
	return NodeState{
		Name: name, CPUs: cpus, MemBytes: memGiB << 30,
		GPUs: gpus, GPUMemBytes: gpuMemGiB << 30, Online: true,
	}
}

func TestPrefersFewestNodes(t *testing.T) {
	// 64 GiB is available either as one big node or three small ones.
	nodes := []NodeState{
		n("small1", 4, 16, 1, 12), n("big", 16, 64, 1, 48), n("small2", 4, 16, 1, 12),
	}
	p, why := SolveAggregate(Aggregate{TotalMemBytes: 48 << 30}, nodes)
	if p == nil {
		t.Fatalf("no placement: %s", why)
	}
	// Every extra node is a network hop on every collective.
	if len(p.Nodes) != 1 || p.Nodes[0].Node != "big" {
		t.Errorf("placement = %v, want just [big]", p.NodeNames())
	}
}

func TestPoolsAcrossNodesWhenNoSingleNodeFits(t *testing.T) {
	nodes := []NodeState{n("a", 8, 16, 1, 12), n("b", 8, 16, 1, 12), n("c", 8, 16, 1, 12)}
	// 30 GiB of GPU memory: no single node has it, three together do.
	p, why := SolveAggregate(Aggregate{TotalGPUMem: 30 << 30}, nodes)
	if p == nil {
		t.Fatalf("should pool across nodes: %s", why)
	}
	if len(p.Nodes) != 3 {
		t.Errorf("used %d nodes (%v), want 3", len(p.Nodes), p.NodeNames())
	}
	var total int64
	for _, s := range p.Nodes {
		total += s.GPUMem
	}
	if total < 30<<30 {
		t.Errorf("placement provides %d GiB, need 30", total>>30)
	}
}

func TestMinimalityDropsRedundantNodes(t *testing.T) {
	// Greedy can pick small nodes first; the minimality pass must drop any
	// that a later, larger node made unnecessary.
	nodes := []NodeState{n("tiny", 1, 1, 0, 0), n("huge", 64, 256, 0, 0)}
	p, why := SolveAggregate(Aggregate{TotalCPUs: 8}, nodes)
	if p == nil {
		t.Fatalf("no placement: %s", why)
	}
	if len(p.Nodes) != 1 || p.Nodes[0].Node != "huge" {
		t.Errorf("placement = %v, want [huge] only", p.NodeNames())
	}
}

func TestImpossibleRequestSaysSoRatherThanWaiting(t *testing.T) {
	nodes := []NodeState{n("a", 8, 16, 1, 12)}
	_, why := SolveAggregate(Aggregate{TotalGPUMem: 500 << 30}, nodes)
	// "will never fit" and "wait your turn" call for different user actions.
	if !strings.Contains(why, "never fit") {
		t.Errorf("explanation = %q; should say it can never fit", why)
	}
	// The request should be echoed in the units the user thinks in.
	if !strings.Contains(why, "500 GiB") {
		t.Errorf("explanation should quote the request as typed: %q", why)
	}
}

func TestBusyClusterSaysWaitNotImpossible(t *testing.T) {
	busy := n("a", 8, 64, 1, 48)
	busy.UsedGPUMem = 40 << 30
	_, why := SolveAggregate(Aggregate{TotalGPUMem: 32 << 30}, []NodeState{busy})
	if strings.Contains(why, "never fit") {
		t.Errorf("capacity exists but is busy; should say waiting, got %q", why)
	}
	if !strings.Contains(why, "waiting") {
		t.Errorf("explanation = %q, want a waiting message", why)
	}
}

func TestDrainedAndOfflineNodesExcludedAndExplained(t *testing.T) {
	d := n("drained", 8, 64, 1, 48)
	d.Drained = true
	off := n("offline", 8, 64, 1, 48)
	off.Online = false
	_, why := SolveAggregate(Aggregate{TotalCPUs: 4}, []NodeState{d, off})
	if !strings.Contains(why, "drained") || !strings.Contains(why, "offline") {
		t.Errorf("explanation should mention both states, got %q", why)
	}
}

func TestMaxNodesRespected(t *testing.T) {
	nodes := []NodeState{n("a", 4, 8, 0, 0), n("b", 4, 8, 0, 0), n("c", 4, 8, 0, 0)}
	_, why := SolveAggregate(Aggregate{TotalCPUs: 12, MaxNodes: 2}, nodes)
	if !strings.Contains(why, "nodes=2") {
		t.Errorf("should refuse to exceed the node ceiling, got %q", why)
	}
	// And it should succeed when the ceiling is sufficient.
	if p, why := SolveAggregate(Aggregate{TotalCPUs: 8, MaxNodes: 2}, nodes); p == nil {
		t.Errorf("should fit within 2 nodes: %s", why)
	}
}

func TestPlacementExplainsItself(t *testing.T) {
	nodes := []NodeState{n("a", 8, 16, 1, 12), n("b", 8, 16, 1, 12)}
	p, _ := SolveAggregate(Aggregate{TotalGPUMem: 20 << 30}, nodes)
	if p == nil {
		t.Fatal("expected a placement")
	}
	for _, want := range []string{"GPU memory", "node"} {
		if !strings.Contains(p.Why, want) {
			t.Errorf("Why = %q, missing %q", p.Why, want)
		}
	}
}

func TestEmptyRequestIsRecognised(t *testing.T) {
	if !(Aggregate{}).Empty() {
		t.Error("zero Aggregate should be Empty")
	}
	if (Aggregate{TotalCPUs: 1}).Empty() {
		t.Error("a request with CPUs is not empty")
	}
}
