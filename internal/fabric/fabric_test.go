package fabric

import (
	"strings"
	"testing"
)

func mac(name, addr string, peers ...string) Node {
	return Node{Name: name, Addr: addr, CPUs: 10, GPUs: 1, GPUKind: "metal",
		GPUMemBytes: 12 << 30, OS: "darwin", Arch: "arm64", OSVersion: "26.4",
		ThunderboltPeers: peers}
}
func linuxGPU(name, addr string) Node {
	return Node{Name: name, Addr: addr, CPUs: 16, GPUs: 1, GPUKind: "cuda",
		GPUMemBytes: 24 << 30, OS: "linux", Arch: "amd64"}
}
func alloc(nodes ...Node) Allocation {
	return Allocation{JobID: 1, Nodes: nodes, MasterAddr: nodes[0].Addr, MasterPort: 29500}
}

func TestDefaultsToNoFabricForIndependentWork(t *testing.T) {
	c, err := Select(alloc(mac("a", "10.0.0.1"), mac("b", "10.0.0.2")), Workload{})
	if err != nil {
		t.Fatal(err)
	}
	// The common case -- a parameter sweep -- must not drag in a runtime.
	if c.Fabric.Name() != "none" {
		t.Errorf("chose %q for independent ranks, want none", c.Fabric.Name())
	}
}

func TestPythonMultiNodeChoosesRay(t *testing.T) {
	c, err := Select(alloc(mac("a", "10.0.0.1"), mac("b", "10.0.0.2")),
		Workload{Python: true, Collective: true})
	if err != nil {
		t.Fatal(err)
	}
	if c.Fabric.Name() != "ray" {
		t.Errorf("chose %q, want ray (considered: %v)", c.Fabric.Name(), c.Considered)
	}
}

func TestAllMacModelChoosesMLX(t *testing.T) {
	c, err := Select(alloc(mac("a", "10.0.0.1", "b"), mac("b", "10.0.0.2", "a")),
		Workload{Model: "qwen3", Collective: true})
	if err != nil {
		t.Fatal(err)
	}
	if c.Fabric.Name() != "mlx" {
		t.Errorf("chose %q, want mlx (considered: %v)", c.Fabric.Name(), c.Considered)
	}
}

func TestMixedHardwareModelChoosesLlamaCppRPC(t *testing.T) {
	// A Mac and a CUDA box: MLX cannot span them, so the heterogeneous
	// sharding fabric is the only option that runs the model at all.
	c, err := Select(alloc(mac("a", "10.0.0.1"), linuxGPU("b", "10.0.0.2")),
		Workload{Model: "qwen3", Collective: true})
	if err != nil {
		t.Fatal(err)
	}
	if c.Fabric.Name() != "llamacpp-rpc" {
		t.Errorf("chose %q, want llamacpp-rpc (considered: %v)", c.Fabric.Name(), c.Considered)
	}
	// The user should be told the trade-off, not just the choice.
	joined := strings.Join(c.Reasons, " ")
	if !strings.Contains(joined, "capacity, not speed") {
		t.Errorf("reasons should state the trade-off, got %v", c.Reasons)
	}
}

func TestSingleNodeNeverPicksADistributedFabric(t *testing.T) {
	for _, w := range []Workload{{}, {Python: true}, {Model: "x"}, {Collective: true}} {
		c, err := Select(alloc(mac("solo", "10.0.0.1")), w)
		if err != nil {
			t.Fatalf("workload %+v: %v", w, err)
		}
		if c.Fabric.Name() != "none" {
			t.Errorf("workload %+v chose %q on one node; want none", w, c.Fabric.Name())
		}
	}
}

func TestMLXPrefersJACCLOnlyWhenFullyThunderbolted(t *testing.T) {
	full := []Node{mac("a", "1", "b"), mac("b", "2", "a")}
	if got := MLXBackend(full); got != "jaccl" {
		t.Errorf("fully TB-connected backend = %q, want jaccl", got)
	}
	// A partial mesh cannot use a Thunderbolt-only transport for the ring.
	partial := []Node{mac("a", "1", "b"), mac("b", "2", "a"), mac("c", "3")}
	if got := MLXBackend(partial); got != "ring" {
		t.Errorf("partial TB mesh backend = %q, want ring", got)
	}
	// Older macOS lacks the RDMA support JACCL needs.
	old := []Node{mac("a", "1", "b"), mac("b", "2", "a")}
	old[0].OSVersion, old[1].OSVersion = "15.0", "15.0"
	if got := MLXBackend(old); got != "ring" {
		t.Errorf("pre-26.2 backend = %q, want ring", got)
	}
}

// The security requirement, as a test.
func TestRPCWorkersBindToMeshAddressOnly(t *testing.T) {
	a := alloc(mac("a", "10.0.0.1"), linuxGPU("b", "10.0.0.2"))
	plans, err := LlamaCppRPC{}.Plan(a, Workload{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	var workers int
	for _, p := range plans {
		if p.Role != RoleWorker {
			continue
		}
		workers++
		cmd := strings.Join(p.Command, " ")
		if !strings.Contains(cmd, "-H 10.0.0.2") {
			t.Errorf("worker does not bind to the mesh address: %q", cmd)
		}
		for _, bad := range []string{"0.0.0.0", "-H ::", "--host 0.0.0.0"} {
			if strings.Contains(cmd, bad) {
				t.Errorf("worker binds a wildcard address (%s): %q", bad, cmd)
			}
		}
	}
	if workers == 0 {
		t.Error("expected at least one rpc-server worker")
	}
}

func TestRPCRefusesNodeWithNoMeshAddress(t *testing.T) {
	bad := linuxGPU("b", "") // no address
	_, err := LlamaCppRPC{}.Plan(alloc(mac("a", "10.0.0.1"), bad), Workload{Model: "m"})
	if err == nil {
		t.Fatal("planning should fail rather than bind an unauthenticated server to a guess")
	}
	if !strings.Contains(err.Error(), "unauthenticated") {
		t.Errorf("error should explain the risk, got %v", err)
	}
}

func TestRayHeadAndWorkers(t *testing.T) {
	a := alloc(mac("a", "10.0.0.1"), mac("b", "10.0.0.2"), mac("c", "10.0.0.3"))
	plans, err := Ray{}.Plan(a, Workload{Python: true})
	if err != nil {
		t.Fatal(err)
	}
	if plans[0].Role != RoleUser {
		t.Error("rank 0 should run the user's script")
	}
	if len(plans[0].Pre) == 0 || !strings.Contains(strings.Join(plans[0].Pre[0], " "), "--head") {
		t.Errorf("rank 0 should start the Ray head, got %v", plans[0].Pre)
	}
	for _, p := range plans[1:] {
		if p.Role != RoleWorker {
			t.Errorf("rank %d should be a worker", p.Rank)
		}
		if !strings.Contains(strings.Join(p.Command, " "), "--block") {
			t.Errorf("worker must block for the job's lifetime, got %v", p.Command)
		}
	}
	// Every rank must tear its scaffolding down, or a failed job leaves a
	// Ray cluster running on someone's machine.
	for _, p := range plans {
		if len(p.Post) == 0 {
			t.Errorf("rank %d has no teardown", p.Rank)
		}
	}
}

func TestExplicitRequestOverridesScoring(t *testing.T) {
	a := alloc(mac("a", "10.0.0.1"), mac("b", "10.0.0.2"))
	c, err := Select(a, Workload{Python: true, Requested: "none"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Fabric.Name() != "none" {
		t.Errorf("explicit request ignored: got %q", c.Fabric.Name())
	}
	// But an impossible request must fail loudly rather than silently fall back.
	if _, err := Select(alloc(mac("solo", "1")), Workload{Requested: "mlx"}); err == nil {
		t.Error("requesting an inapplicable fabric should error")
	}
	if _, err := Select(a, Workload{Requested: "nonsense"}); err == nil {
		t.Error("unknown fabric name should error")
	}
}

func TestSelectionIsExplainable(t *testing.T) {
	c, err := Select(alloc(mac("a", "10.0.0.1"), linuxGPU("b", "10.0.0.2")),
		Workload{Model: "m", Collective: true})
	if err != nil {
		t.Fatal(err)
	}
	// Every alternative should be listed with its verdict; an automatic choice
	// the user cannot interrogate is a black box.
	if len(c.Considered) < 4 {
		t.Errorf("only %d alternatives recorded: %v", len(c.Considered), c.Considered)
	}
	for _, name := range []string{"none", "ray", "mlx", "llamacpp-rpc"} {
		found := false
		for _, s := range c.Considered {
			if strings.HasPrefix(s, name) {
				found = true
			}
		}
		if !found {
			t.Errorf("%q missing from the considered list", name)
		}
	}
}

func TestMLXHostfileShape(t *testing.T) {
	got := MLXHostfile([]Node{mac("a", "10.0.0.1"), mac("b", "10.0.0.2")})
	for _, want := range []string{`"ssh": "10.0.0.1"`, `"ssh": "10.0.0.2"`, "[", "]"} {
		if !strings.Contains(got, want) {
			t.Errorf("hostfile missing %q:\n%s", want, got)
		}
	}
}
