// Package fabric decides how the ranks of a multi-node job coordinate.
//
// The plan's central claim is that the primitive is "allocate an aggregate
// resource pool and let the workload coordinate across it" -- not "shard a
// model". Coordination is therefore a pluggable concern, and the most common
// backend does nothing at all: task-parallel work and DAG stages need only
// scheduling and rank environment, no collectives.
//
// Backends are scored rather than configured. A user asks for capacity; making
// them also name a distribution runtime would defeat the point.
package fabric

import (
	"fmt"
	"sort"
	"strings"
)

// Node is one machine in an allocation.
type Node struct {
	Name        string
	Addr        string
	CPUs        int
	GPUs        int
	GPUKind     string // "metal" | "cuda" | "rocm" | ""
	GPUMemBytes int64
	OS          string
	Arch        string
	// ThunderboltPeers are nodes reachable over Thunderbolt. Bandwidth
	// dominates collective cost on a home network, so this changes placement
	// quality materially.
	ThunderboltPeers []string
	// OSVersion lets a backend gate on a feature (JACCL needs macOS 26.2+).
	OSVersion string
}

func (n Node) IsAppleSilicon() bool { return n.OS == "darwin" && n.Arch == "arm64" }

// Workload describes what is being run, so a fabric can judge its own fit.
type Workload struct {
	// Model, when set, means the job is model-parallel inference: it needs the
	// weights split across nodes rather than N independent copies.
	Model string
	// Python is true when the job's entry point is a Python program; Ray only
	// makes sense then.
	Python bool
	// Collective is true when ranks must exchange data during the run.
	// Task-parallel work leaves it false and needs no fabric at all.
	Collective bool
	// Requested pins a backend by name, bypassing scoring.
	Requested string
}

// Allocation is the set of nodes a job was given.
type Allocation struct {
	JobID      int64
	Nodes      []Node
	MasterAddr string
	MasterPort int
}

func (a Allocation) Names() []string {
	out := make([]string, 0, len(a.Nodes))
	for _, n := range a.Nodes {
		out = append(out, n.Name)
	}
	return out
}

// Role distinguishes ranks that run the user's program from ranks that exist
// only to serve other ranks.
type Role string

const (
	// RoleUser runs the submitted script.
	RoleUser Role = "user"
	// RoleWorker runs infrastructure -- an RPC server or a Ray worker -- and
	// never executes the user's script. Its lifetime is the job's.
	RoleWorker Role = "worker"
)

// RankPlan is what one rank actually does.
type RankPlan struct {
	Rank int
	Node string
	Role Role
	Env  map[string]string
	// Command replaces the user's script when non-empty, for worker ranks.
	Command []string
	// Pre runs before the rank's main command; Post runs after, always,
	// so a fabric can tear its own scaffolding down.
	Pre  [][]string
	Post [][]string
}

// Fabric plans coordination for one allocation.
type Fabric interface {
	Name() string
	// Suitability scores this fabric for the allocation, higher is better.
	// A score <= 0 means "not applicable"; reasons explain the verdict to the
	// user, since an automatic choice they cannot interrogate is a black box.
	Suitability(Allocation, Workload) (int, []string)
	Plan(Allocation, Workload) ([]RankPlan, error)
}

// All returns the registered backends in preference order for ties.
func All() []Fabric {
	return []Fabric{
		&None{}, &Ray{}, &MLX{}, &LlamaCppRPC{},
	}
}

// Choice records which fabric was selected and why.
type Choice struct {
	Fabric  Fabric
	Score   int
	Reasons []string
	// Considered lists the alternatives and their verdicts.
	Considered []string
}

// Select picks the best-scoring fabric for an allocation.
func Select(a Allocation, w Workload) (Choice, error) {
	type scored struct {
		f       Fabric
		score   int
		reasons []string
	}
	var all []scored
	for _, f := range All() {
		s, r := f.Suitability(a, w)
		all = append(all, scored{f, s, r})
	}
	if w.Requested != "" {
		for _, s := range all {
			if s.f.Name() != w.Requested {
				continue
			}
			if s.score <= 0 {
				return Choice{}, fmt.Errorf("fabric %q is not usable for this allocation: %s",
					w.Requested, strings.Join(s.reasons, "; "))
			}
			return Choice{Fabric: s.f, Score: s.score, Reasons: s.reasons}, nil
		}
		return Choice{}, fmt.Errorf("unknown fabric %q", w.Requested)
	}
	// Stable order so the same cluster state always yields the same choice.
	sort.SliceStable(all, func(i, j int) bool { return all[i].score > all[j].score })

	best := all[0]
	if best.score <= 0 {
		return Choice{}, fmt.Errorf("no fabric fits this allocation")
	}
	c := Choice{Fabric: best.f, Score: best.score, Reasons: best.reasons}
	for _, s := range all {
		verdict := fmt.Sprintf("%s (score %d)", s.f.Name(), s.score)
		if len(s.reasons) > 0 {
			verdict += ": " + strings.Join(s.reasons, ", ")
		}
		c.Considered = append(c.Considered, verdict)
	}
	return c, nil
}

// homogeneous reports whether every node shares an OS, arch and GPU kind.
func homogeneous(nodes []Node) bool {
	if len(nodes) < 2 {
		return true
	}
	f := nodes[0]
	for _, n := range nodes[1:] {
		if n.OS != f.OS || n.Arch != f.Arch || n.GPUKind != f.GPUKind {
			return false
		}
	}
	return true
}

// allThunderbolt reports whether every node is Thunderbolt-connected to every
// other. Only then can a Thunderbolt-only transport be used for the whole ring.
func allThunderbolt(nodes []Node) bool {
	if len(nodes) < 2 {
		return false
	}
	for _, n := range nodes {
		peers := map[string]bool{}
		for _, p := range n.ThunderboltPeers {
			peers[p] = true
		}
		for _, other := range nodes {
			if other.Name == n.Name {
				continue
			}
			if !peers[other.Name] {
				return false
			}
		}
	}
	return true
}
