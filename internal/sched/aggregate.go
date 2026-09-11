package sched

import (
	"fmt"
	"sort"
	"strings"

	"github.com/davidwu/shome/internal/job"
)

// Aggregate is a request for totals across however many nodes it takes,
// rather than a per-node shape the user worked out themselves.
//
// This is the inversion that makes pooling usable: `--total-gpu-mem 132G`
// instead of figuring out which four machines add up to 132 GiB and passing
// -N4 with a hand-computed per-node figure.
type Aggregate struct {
	// Constraint restricts which nodes may be pooled.
	Constraint string

	TotalCPUs     int
	TotalMemBytes int64
	TotalGPUMem   int64
	TotalGPUs     int
	MaxNodes      int // 0 = no ceiling
}

// Empty reports whether no aggregate request was made.
func (a Aggregate) Empty() bool {
	return a.TotalCPUs == 0 && a.TotalMemBytes == 0 && a.TotalGPUMem == 0 && a.TotalGPUs == 0
}

// Placement is a solved multi-node allocation.
type Placement struct {
	Nodes []NodeShare
	// Why explains the choice in user-facing terms. Home clusters are opaque
	// enough without a scheduler that will not say what it did.
	Why string
}

// NodeShare is one node's contribution to a placement.
type NodeShare struct {
	Node     string
	CPUs     int
	MemBytes int64
	GPUs     int
	GPUMem   int64
}

func (p Placement) NodeNames() []string {
	out := make([]string, 0, len(p.Nodes))
	for _, n := range p.Nodes {
		out = append(out, n.Node)
	}
	return out
}

// SolveAggregate finds the smallest set of nodes whose free capacity satisfies
// the request.
//
// Fewest nodes first, deliberately. Every extra node in a distributed job adds
// a network hop to every collective, and on a home network that cost dominates
// -- two machines over Thunderbolt will beat four over Wi-Fi for the same
// total memory. Among equal-sized sets, prefer the better-connected nodes.
//
// Returns a nil placement and an explanation when the request cannot be met.
func SolveAggregate(req Aggregate, nodes []NodeState) (*Placement, string) {
	avail := make([]NodeState, 0, len(nodes))
	var drained, offline, unmatched int
	var constraint *Constraint
	if req.Constraint != "" {
		var err error
		if constraint, err = ParseConstraint(req.Constraint); err != nil {
			return nil, "invalid --constraint: " + err.Error()
		}
	}
	for _, n := range nodes {
		if constraint != nil && !constraint.Matches(n.Features) {
			unmatched++
			continue
		}
		if n.Drained {
			drained++
			continue
		}
		if !n.Online {
			offline++
			continue
		}
		avail = append(avail, n)
	}
	if len(avail) == 0 {
		if unmatched > 0 {
			return nil, fmt.Sprintf("no node satisfies --constraint=%q (%d node(s) checked)",
				req.Constraint, unmatched)
		}
		return nil, explainNoNodes(drained, offline)
	}

	// Greedy by descending contribution: take the node that closes the most of
	// the remaining gap first. Optimal set cover is NP-hard and a home cluster
	// has a handful of nodes, so greedy plus a minimality pass is ample.
	sort.Slice(avail, func(i, j int) bool {
		return contribution(req, avail[i]) > contribution(req, avail[j])
	})

	var chosen []NodeState
	need := req
	for _, n := range avail {
		if satisfied(need) {
			break
		}
		if req.MaxNodes > 0 && len(chosen) >= req.MaxNodes {
			break
		}
		if contribution(req, n) == 0 {
			continue // nothing this node offers is wanted
		}
		chosen = append(chosen, n)
		need = subtract(need, n)
	}
	if !satisfied(need) {
		return nil, explainShortfall(req, need, avail, req.MaxNodes)
	}

	// Minimality: drop any node the set no longer needs. Greedy can overshoot
	// when a later large node subsumes earlier small ones.
	for i := 0; i < len(chosen); {
		trial := append(append([]NodeState{}, chosen[:i]...), chosen[i+1:]...)
		if satisfied(subtractAll(req, trial)) {
			chosen = trial
			continue
		}
		i++
	}

	p := &Placement{}
	rem := req
	for _, n := range chosen {
		share := NodeShare{Node: n.Name}
		share.CPUs = min(max(rem.TotalCPUs, 0), n.freeCPUs())
		share.MemBytes = min(max(rem.TotalMemBytes, 0), n.freeMem())
		share.GPUs = min(max(rem.TotalGPUs, 0), n.freeGPUs())
		share.GPUMem = min(max(rem.TotalGPUMem, 0), n.freeGPUMem())
		p.Nodes = append(p.Nodes, share)
		rem = subtract(rem, n)
	}
	p.Why = describe(req, p, len(avail))
	return p, ""
}

// hbytes formats a byte count the way the user most likely typed it, so an
// error quotes "500 GiB" back rather than "512000 MiB".
func hbytes(b int64) string {
	switch {
	case b >= 1<<40 && b%(1<<40) == 0:
		return fmt.Sprintf("%d TiB", b>>40)
	case b >= 1<<30:
		if b%(1<<30) == 0 {
			return fmt.Sprintf("%d GiB", b>>30)
		}
		return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%d MiB", b>>20)
	default:
		return fmt.Sprintf("%d B", b)
	}
}

func contribution(req Aggregate, n NodeState) int64 {
	var c int64
	if req.TotalCPUs > 0 {
		c += int64(n.freeCPUs())
	}
	if req.TotalMemBytes > 0 {
		c += n.freeMem() >> 20
	}
	if req.TotalGPUMem > 0 {
		c += n.freeGPUMem() >> 20
	}
	if req.TotalGPUs > 0 {
		c += int64(n.freeGPUs()) * 1000
	}
	return c
}

func satisfied(need Aggregate) bool {
	return need.TotalCPUs <= 0 && need.TotalMemBytes <= 0 &&
		need.TotalGPUMem <= 0 && need.TotalGPUs <= 0
}

func subtract(need Aggregate, n NodeState) Aggregate {
	need.TotalCPUs -= n.freeCPUs()
	need.TotalMemBytes -= n.freeMem()
	need.TotalGPUMem -= n.freeGPUMem()
	need.TotalGPUs -= n.freeGPUs()
	return need
}

func subtractAll(req Aggregate, ns []NodeState) Aggregate {
	for _, n := range ns {
		req = subtract(req, n)
	}
	return req
}

func describe(req Aggregate, p *Placement, availCount int) string {
	var parts []string
	if req.TotalCPUs > 0 {
		parts = append(parts, fmt.Sprintf("%d CPUs", req.TotalCPUs))
	}
	if req.TotalMemBytes > 0 {
		parts = append(parts, fmt.Sprintf("%s memory", hbytes(req.TotalMemBytes)))
	}
	if req.TotalGPUMem > 0 {
		parts = append(parts, fmt.Sprintf("%s GPU memory", hbytes(req.TotalGPUMem)))
	}
	if req.TotalGPUs > 0 {
		parts = append(parts, fmt.Sprintf("%d GPUs", req.TotalGPUs))
	}
	return fmt.Sprintf("need %s; smallest set that fits is %d of %d available node(s): %s",
		strings.Join(parts, " + "), len(p.Nodes), availCount, strings.Join(p.NodeNames(), ", "))
}

func explainNoNodes(drained, offline int) string {
	switch {
	case drained > 0 && offline > 0:
		return fmt.Sprintf("no nodes available (%d drained by their owners, %d offline)", drained, offline)
	case drained > 0:
		return fmt.Sprintf("no nodes available (%d drained by their owners)", drained)
	case offline > 0:
		return fmt.Sprintf("no nodes available (%d offline)", offline)
	default:
		return "no nodes registered"
	}
}

// explainShortfall says how far short the cluster is, and by which resource --
// the difference between "wait" and "this will never fit" matters.
func explainShortfall(req, need Aggregate, avail []NodeState, maxNodes int) string {
	var whole, free Aggregate
	for _, n := range avail {
		whole.TotalCPUs += n.CPUs
		whole.TotalMemBytes += n.MemBytes
		whole.TotalGPUMem += n.GPUMemBytes
		whole.TotalGPUs += n.GPUs
		free.TotalCPUs += n.freeCPUs()
		free.TotalMemBytes += n.freeMem()
		free.TotalGPUMem += n.freeGPUMem()
		free.TotalGPUs += n.freeGPUs()
	}
	if maxNodes > 0 && len(avail) > maxNodes {
		return fmt.Sprintf("cannot fit within --nodes=%d; the request needs more machines", maxNodes)
	}
	switch {
	case req.TotalCPUs > whole.TotalCPUs:
		return fmt.Sprintf("the whole cluster has %d CPUs; %d requested -- this will never fit",
			whole.TotalCPUs, req.TotalCPUs)
	case req.TotalMemBytes > whole.TotalMemBytes:
		return fmt.Sprintf("the whole cluster has %s of memory; %s requested -- this will never fit",
			hbytes(whole.TotalMemBytes), hbytes(req.TotalMemBytes))
	case req.TotalGPUMem > whole.TotalGPUMem:
		return fmt.Sprintf("the whole cluster has %s of GPU memory; %s requested -- this will never fit",
			hbytes(whole.TotalGPUMem), hbytes(req.TotalGPUMem))
	case req.TotalGPUs > whole.TotalGPUs:
		return fmt.Sprintf("the whole cluster has %d GPUs; %d requested -- this will never fit",
			whole.TotalGPUs, req.TotalGPUs)
	}
	return fmt.Sprintf("not enough free capacity right now (free: %d CPUs, %s memory, %s GPU memory) -- waiting",
		free.TotalCPUs, hbytes(free.TotalMemBytes), hbytes(free.TotalGPUMem))
}

// AggregateFromSpec reads the aggregate request off a job spec.
func AggregateFromSpec(s job.Spec) Aggregate {
	return Aggregate{
		TotalCPUs:     s.TotalCPUs,
		TotalMemBytes: s.TotalMemBytes,
		TotalGPUMem:   s.TotalGPUMem,
		TotalGPUs:     s.TotalGPUs,
		MaxNodes:      s.MaxNodes,
		Constraint:    s.Constraint,
	}
}
