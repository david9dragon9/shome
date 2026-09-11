package fabric

import (
	"fmt"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------- none

// None is rank launch and environment wiring, nothing else.
//
// This is the default and the most common case by a wide margin: parameter
// sweeps, job arrays, per-shard data processing and DAG stages all run N
// independent processes that never speak to each other. Giving them a
// collective runtime would add failure modes and buy nothing.
type None struct{}

func (None) Name() string { return "none" }

func (None) Suitability(a Allocation, w Workload) (int, []string) {
	if w.Collective || w.Model != "" {
		// Still usable -- the script may do its own coordination over the
		// addresses shome exports -- but anything purpose-built beats it.
		return 10, []string{"no collectives provided; the job must coordinate itself"}
	}
	return 100, []string{"independent ranks need no coordination runtime"}
}

func (None) Plan(a Allocation, w Workload) ([]RankPlan, error) {
	out := make([]RankPlan, 0, len(a.Nodes))
	for i, n := range a.Nodes {
		out = append(out, RankPlan{Rank: i, Node: n.Name, Role: RoleUser, Env: map[string]string{}})
	}
	return out, nil
}

// ---------------------------------------------------------------- ray

// Ray covers the widest range of Python workloads: task and actor parallelism,
// DAGs, out-of-core data and resident services. Rank 0 runs the head; every
// other rank runs a worker that joins it and then idles for the job's life.
type Ray struct{}

func (Ray) Name() string { return "ray" }

const rayPort = 6379

func (Ray) Suitability(a Allocation, w Workload) (int, []string) {
	if len(a.Nodes) < 2 {
		return 0, []string{"single node: a Ray cluster adds overhead with nothing to gain"}
	}
	if !w.Python {
		return 0, []string{"not a Python workload"}
	}
	if w.Model != "" {
		return 30, []string{"usable, but a sharding fabric fits model-parallel inference better"}
	}
	return 80, []string{"Python workload across several nodes"}
}

func (Ray) Plan(a Allocation, w Workload) ([]RankPlan, error) {
	if len(a.Nodes) < 2 {
		return nil, fmt.Errorf("ray needs at least two nodes")
	}
	addr := fmt.Sprintf("%s:%d", a.MasterAddr, rayPort)
	out := make([]RankPlan, 0, len(a.Nodes))
	for i, n := range a.Nodes {
		env := map[string]string{
			"RAY_ADDRESS":       addr,
			"SHOME_RAY_ADDRESS": addr,
			// Ray writes large spill files; keep them in the job's scratch so
			// they are removed with it rather than filling the owner's disk.
			"RAY_TMPDIR": "$SHOME_SCRATCH/ray",
		}
		cpus := strconv.Itoa(n.CPUs)
		if i == 0 {
			out = append(out, RankPlan{
				Rank: i, Node: n.Name, Role: RoleUser, Env: env,
				Pre: [][]string{{"ray", "start", "--head",
					"--port", strconv.Itoa(rayPort), "--num-cpus", cpus}},
				Post: [][]string{{"ray", "stop", "--force"}},
			})
			continue
		}
		// Workers exist only to serve the head. They block until the job ends,
		// which is what keeps the cluster alive for rank 0's script.
		out = append(out, RankPlan{
			Rank: i, Node: n.Name, Role: RoleWorker, Env: env,
			Command: []string{"ray", "start", "--address", addr,
				"--num-cpus", cpus, "--block"},
			Post: [][]string{{"ray", "stop", "--force"}},
		})
	}
	return out, nil
}

// ---------------------------------------------------------------- mlx

// MLX is the fastest path for Mac-to-Mac work. It is Apple-only by
// construction, so it scores zero on any mixed allocation.
type MLX struct{}

func (MLX) Name() string { return "mlx" }

func (MLX) Suitability(a Allocation, w Workload) (int, []string) {
	if len(a.Nodes) < 2 {
		return 0, []string{"single node: nothing to distribute"}
	}
	for _, n := range a.Nodes {
		if !n.IsAppleSilicon() {
			return 0, []string{"MLX runs only on Apple Silicon; this allocation is mixed"}
		}
	}
	// All-Mac is necessary but not sufficient. MLX only helps a program that
	// actually uses MLX, so an ordinary Python job on Macs belongs to Ray --
	// handing it MLX would wire up a runtime it never calls.
	if w.Model == "" {
		return 0, []string{"all-Apple-Silicon, but MLX only applies to model-parallel work; " +
			"pass --fabric=mlx to force it"}
	}
	reasons := []string{"all-Apple-Silicon allocation"}
	score := 90
	if allThunderbolt(a.Nodes) {
		// RDMA over Thunderbolt is roughly an order of magnitude lower latency
		// than the TCP ring, which is the difference between usable and not.
		score = 95
		reasons = append(reasons, "fully Thunderbolt-connected: JACCL backend available")
	} else {
		reasons = append(reasons, "not fully Thunderbolt-connected: falling back to the ring backend")
	}
	return score, reasons
}

// MLXBackend picks between JACCL and ring for an allocation.
func MLXBackend(nodes []Node) string {
	if allThunderbolt(nodes) && allAtLeastMacOS(nodes, 26, 2) {
		return "jaccl"
	}
	return "ring"
}

func (m MLX) Plan(a Allocation, w Workload) ([]RankPlan, error) {
	if len(a.Nodes) < 2 {
		return nil, fmt.Errorf("mlx needs at least two nodes")
	}
	backend := MLXBackend(a.Nodes)
	hosts := make([]string, 0, len(a.Nodes))
	for _, n := range a.Nodes {
		h := n.Addr
		if h == "" {
			h = n.Name
		}
		hosts = append(hosts, h)
	}
	out := make([]RankPlan, 0, len(a.Nodes))
	for i, n := range a.Nodes {
		out = append(out, RankPlan{
			Rank: i, Node: n.Name, Role: RoleUser,
			Env: map[string]string{
				"SHOME_MLX_BACKEND": backend,
				"SHOME_MLX_HOSTS":   strings.Join(hosts, ","),
				// mlx.launch reads a hostfile; the agent writes it into the
				// job's scratch from SHOME_MLX_HOSTS.
				"SHOME_MLX_HOSTFILE": "$SHOME_SCRATCH/hostfile.json",
				"MLX_RANK":           strconv.Itoa(i),
				"MLX_WORLD_SIZE":     strconv.Itoa(len(a.Nodes)),
			},
		})
	}
	return out, nil
}

// MLXHostfile renders the hostfile.json that mlx.launch expects.
func MLXHostfile(nodes []Node) string {
	var b strings.Builder
	b.WriteString("[\n")
	for i, n := range nodes {
		h := n.Addr
		if h == "" {
			h = n.Name
		}
		fmt.Fprintf(&b, "  {\"ssh\": %q}", h)
		if i < len(nodes)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	b.WriteString("]\n")
	return b.String()
}

func allAtLeastMacOS(nodes []Node, major, minor int) bool {
	for _, n := range nodes {
		maj, min, ok := parseVersion(n.OSVersion)
		if !ok || maj < major || (maj == major && min < minor) {
			return false
		}
	}
	return true
}

func parseVersion(v string) (major, minor int, ok bool) {
	parts := strings.Split(v, ".")
	if len(parts) == 0 || parts[0] == "" {
		return 0, 0, false
	}
	var err error
	if major, err = strconv.Atoi(parts[0]); err != nil {
		return 0, 0, false
	}
	if len(parts) > 1 {
		minor, _ = strconv.Atoi(parts[1])
	}
	return major, minor, true
}

// ---------------------------------------------------------------- llama.cpp RPC

// LlamaCppRPC is the only mature fabric that shards one model across mixed
// hardware -- Metal, CUDA and CPU together. Upstream is blunt that its value
// is capacity, not speed: it makes a model run that otherwise could not.
//
// SECURITY: rpc-server has no authentication whatsoever. It executes tensor
// operations for anyone who can reach the port, as whoever launched it. shome
// therefore CONTAINS it: workers bind to the mesh address only, never
// 0.0.0.0, and run inside the job's sandbox with the job's lifetime. This is a
// requirement, not a hardening option.
type LlamaCppRPC struct{}

func (LlamaCppRPC) Name() string { return "llamacpp-rpc" }

// RPCBasePort is where rank N's rpc-server listens.
const RPCBasePort = 50052

func (LlamaCppRPC) Suitability(a Allocation, w Workload) (int, []string) {
	if len(a.Nodes) < 2 {
		return 0, []string{"single node: no sharding needed"}
	}
	if w.Model == "" {
		return 0, []string{"only applies to model-parallel inference"}
	}
	if homogeneous(a.Nodes) {
		// A purpose-built fabric will beat it on identical hardware.
		return 40, []string{"homogeneous allocation: a native fabric will be faster"}
	}
	return 85, []string{"mixed hardware: the only fabric that shards across differing accelerators",
		"expect capacity, not speed -- throughput is bounded by the slowest link"}
}

func (LlamaCppRPC) Plan(a Allocation, w Workload) ([]RankPlan, error) {
	if len(a.Nodes) < 2 {
		return nil, fmt.Errorf("llamacpp-rpc needs at least two nodes")
	}
	var servers []string
	for i, n := range a.Nodes {
		if i == 0 {
			continue // rank 0 runs the model and drives the others
		}
		addr := n.Addr
		if addr == "" {
			addr = n.Name
		}
		servers = append(servers, fmt.Sprintf("%s:%d", addr, RPCBasePort))
	}
	out := make([]RankPlan, 0, len(a.Nodes))
	for i, n := range a.Nodes {
		if i == 0 {
			out = append(out, RankPlan{
				Rank: i, Node: n.Name, Role: RoleUser,
				Env: map[string]string{
					"SHOME_RPC_SERVERS": strings.Join(servers, ","),
					"SHOME_MODEL":       w.Model,
				},
			})
			continue
		}
		bind := n.Addr
		if bind == "" {
			// Refuse to guess. Binding rpc-server to a wildcard address would
			// expose an unauthenticated execution endpoint to the whole
			// network, so a missing mesh address is a hard error.
			return nil, fmt.Errorf("node %s has no mesh address; refusing to start an "+
				"unauthenticated rpc-server without one", n.Name)
		}
		out = append(out, RankPlan{
			Rank: i, Node: n.Name, Role: RoleWorker,
			Env: map[string]string{"SHOME_RPC_BIND": bind},
			// -H binds to the mesh interface only.
			Command: []string{"rpc-server", "-H", bind, "-p", strconv.Itoa(RPCBasePort)},
		})
	}
	return out, nil
}
