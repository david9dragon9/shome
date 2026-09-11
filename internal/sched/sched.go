// Package sched decides which pending jobs start now.
//
// Plan is a pure function of (pending jobs, node state). No clock, no I/O, no
// daemons -- which is what makes the subtle scheduling bugs testable.
package sched

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/davidwu/shome/internal/job"
)

// NodeState is a node's capacity and current commitment.
type NodeState struct {
	Name string
	// Label is what a person calls this machine, for anything that ends up
	// in front of one. Placement always uses Name -- the certificate
	// identity -- so a display name can never change where a job lands.
	// Empty falls back to Name.
	Label    string
	CPUs     int
	MemBytes int64
	GPUs     int

	UsedCPUs int
	UsedMem  int64
	UsedGPUs int

	Drained bool   // owner paused the node; nothing new may start
	Online  bool   // heartbeating
	Addr    string // reachable address, for gang rendezvous

	GPUMemBytes int64
	UsedGPUMem  int64

	// Features is what --constraint matches against.
	Features NodeFeatures

	// Running is what is on this node now, and when each piece is expected
	// to finish. Needed only for backfill: to know whether a small job can
	// slip into a gap without delaying the job the gap is being held for,
	// the scheduler has to know when the gap opens.
	Running []RunningJob
}

// RunningJob is a job occupying capacity, and when it should end.
type RunningJob struct {
	ID       int64
	CPUs     int
	MemBytes int64
	GPUs     int
	// EndsAt is when its time limit expires. Zero means it has no time limit,
	// so nothing can be predicted about when its capacity comes back.
	EndsAt time.Time
}

// shown is this machine's name as a person should read it.
func (n NodeState) shown() string {
	if n.Label != "" {
		return n.Label
	}
	return n.Name
}

func (n NodeState) freeCPUs() int     { return n.CPUs - n.UsedCPUs }
func (n NodeState) freeMem() int64    { return n.MemBytes - n.UsedMem }
func (n NodeState) freeGPUs() int     { return n.GPUs - n.UsedGPUs }
func (n NodeState) freeGPUMem() int64 { return n.GPUMemBytes - n.UsedGPUMem }

// Decision is either a placement (Node set) or an explanation (Reason set).
// The explanation is not decoration: "why is my job pending?" is the single
// most-wanted missing feature in Slurm, so the scheduler always records it.
type Decision struct {
	JobID  int64
	Node   string
	Reason string
}

func (d Decision) Scheduled() bool { return d.Node != "" }

// Fits reports whether a job's limits fit in a node's free capacity.
func Fits(j *job.Job, n NodeState) (bool, string) {
	if n.Drained {
		return false, "node is drained by its owner"
	}
	// A pinned job may only run where it was told to.
	if len(j.Spec.NodeList) > 0 {
		allowed := false
		for _, want := range j.Spec.NodeList {
			if want == n.Name {
				allowed = true
				break
			}
		}
		if !allowed {
			return false, fmt.Sprintf("not in --nodelist=%s", strings.Join(j.Spec.NodeList, ","))
		}
	}

	// Capability match before capacity: telling someone their job is "waiting
	// for CPUs" when no node could ever satisfy its constraint is misleading.
	if j.Spec.Constraint != "" {
		c, err := ParseConstraint(j.Spec.Constraint)
		if err != nil {
			return false, "invalid --constraint: " + err.Error()
		}
		if !c.Matches(n.Features) {
			return false, fmt.Sprintf("node does not satisfy --constraint=%q", j.Spec.Constraint)
		}
	}
	cpus := j.Spec.Limits.CPUs
	if cpus <= 0 {
		cpus = 1
	}
	if cpus > n.CPUs {
		return false, fmt.Sprintf("requests %d CPUs, node has %d", cpus, n.CPUs)
	}
	if j.Spec.Limits.MemBytes > n.MemBytes {
		return false, fmt.Sprintf("requests %d MiB, node has %d MiB", j.Spec.Limits.MemBytes>>20, n.MemBytes>>20)
	}
	if j.Spec.Limits.GPUs > n.GPUs {
		return false, fmt.Sprintf("requests %d GPUs, node has %d", j.Spec.Limits.GPUs, n.GPUs)
	}
	if cpus > n.freeCPUs() {
		return false, "waiting for CPUs to free"
	}
	if j.Spec.Limits.MemBytes > n.freeMem() {
		return false, "waiting for memory to free"
	}
	if j.Spec.Limits.GPUs > n.freeGPUs() {
		return false, "waiting for a GPU to free"
	}
	return true, ""
}

// Plan assigns pending jobs to nodes, strict FIFO.
//
// Strict means: if the head of the queue does not fit, scheduling stops rather
// than letting smaller jobs jump ahead. That prevents starving large jobs,
// which on a home cluster (few nodes, highly variable job sizes) is a real
// risk. Backfill -- running a later job only when it provably will not delay
// the head -- is a later milestone; doing it correctly needs runtime estimates
// we do not have yet.
// Options tune how Plan orders and packs a round.
//
// The zero value is strict submission order with head-of-line blocking,
// which is what shome did before priority existed and what a single-user
// cluster still wants. Everything here is off unless an admin turns it on.
type Options struct {
	// Backfill lets a lower-priority job start in a gap, provided it can be
	// shown it will finish before the job the gap is being held for.
	Backfill bool

	// Depth is how many blocked jobs get a reservation. Jobs beyond that
	// simply wait, which bounds the work this does on a long queue.
	Depth int

	// Now is the clock for reservations. Zero disables time reasoning, so
	// backfill will only use capacity no reservation wants.
	Now time.Time
}

// Plan decides which pending jobs start now, in strict submission order.
//
// Pending must already be in the order the caller wants considered; when
// priority is enabled the controller sorts it first.
func Plan(pending []*job.Job, nodes []NodeState) []Decision {
	return PlanWith(pending, nodes, Options{})
}

// PlanWith is Plan with backfill.
//
// # What backfill is for
//
// Without it, one job needing eight GPUs at the head of the queue stalls
// every one-CPU job behind it, on idle machines, for as long as it waits.
// That is the thing that makes strict FIFO intolerable on a shared cluster,
// and it is not fixed by priority ordering -- priority only changes *which*
// job does the stalling.
//
// # Why it needs a reservation rather than just "run anything that fits"
//
// Letting small jobs run whenever there is room starves the large job
// forever: there is always another small job. So the blocked job gets a
// reservation -- the time its resources are expected to free up -- and a
// later job may only use those resources if it will demonstrably be finished
// by then. That is Slurm's conservative backfill, and the "demonstrably" is
// load-bearing: a job with no time limit can never be shown to finish in
// time, so it does not get to jump the queue.
func PlanWith(pending []*job.Job, nodes []NodeState, opt Options) []Decision {
	// Work on a copy: callers must be able to plan speculatively.
	ns := make([]NodeState, len(nodes))
	copy(ns, nodes)

	out := make([]Decision, 0, len(pending))

	// reserved holds, per node, the time capacity is being kept free from.
	// A job may still start on a reserved node if it ends before that.
	reserved := map[string]time.Time{}
	depth := opt.Depth
	if depth <= 0 {
		depth = 1
	}
	reservations := 0
	blocked := false

	for _, j := range pending {
		if blocked {
			out = append(out, Decision{JobID: j.ID, Reason: "queued behind an earlier job"})
			continue
		}
		placed := false
		// Two kinds of reason, kept apart on purpose. "No capacity" and "that
		// machine is being held for someone else" call for different actions
		// from the user -- the second is fixable by adding --time -- so the
		// held reason is preferred rather than letting whichever node
		// happened to be examined first decide the message.
		var why, heldWhy string
		for i := range ns {
			ok, reason := Fits(j, ns[i])
			if !ok {
				if why == "" {
					why = reason
				}
				continue
			}
			// The node has room, but it may be room somebody is waiting for.
			if until, held := reserved[ns[i].Name]; held {
				if !endsBefore(j, opt.Now, until) {
					if heldWhy == "" {
						heldWhy = reservationReason(ns[i].shown(), until, j)
					}
					continue
				}
			}
			commit(&ns[i], j)
			out = append(out, Decision{JobID: j.ID, Node: ns[i].Name})
			placed = true
			break
		}
		if placed {
			continue
		}
		if heldWhy != "" {
			why = heldWhy
		}
		if why == "" {
			why = "no nodes available"
		}
		if !opt.Backfill {
			out = append(out, Decision{JobID: j.ID, Reason: why})
			blocked = true // strict FIFO
			continue
		}
		// Hold a place for this job so the jobs behind it cannot starve it,
		// then keep going rather than stalling the whole queue.
		if reservations < depth {
			if node, label, at, ok := reserve(j, ns, reserved, opt.Now); ok {
				reserved[node] = at
				reservations++
				why = fmt.Sprintf("%s; holding %s from %s",
					why, label, at.Format("15:04:05"))
			} else {
				// Nothing can be predicted -- the machines that could run it
				// are occupied by jobs with no time limit, or none is big
				// enough. Falling back to blocking would stall the queue on
				// a job that may never be schedulable, so the queue moves on
				// and this job's age factor is what eventually rescues it.
				why += "; cannot predict when (jobs ahead have no time limit)"
			}
		}
		out = append(out, Decision{JobID: j.ID, Reason: why})
	}
	return out
}

// commit charges a job against a node's free capacity.
func commit(n *NodeState, j *job.Job) {
	cpus := j.Spec.Limits.CPUs
	if cpus <= 0 {
		cpus = 1
	}
	n.UsedCPUs += cpus
	n.UsedMem += j.Spec.Limits.MemBytes
	n.UsedGPUs += j.Spec.Limits.GPUs
}

// endsBefore reports whether j can be shown to finish by `until`.
//
// A job with no time limit cannot, ever. That is the whole safety property of
// backfill: without it, an unbounded job would slip into a reserved gap and
// hold it indefinitely, which is exactly the starvation the reservation
// exists to prevent.
func endsBefore(j *job.Job, now, until time.Time) bool {
	if now.IsZero() || until.IsZero() {
		return false
	}
	if j.Spec.Limits.Walltime <= 0 {
		return false
	}
	return now.Add(j.Spec.Limits.Walltime).Before(until) ||
		now.Add(j.Spec.Limits.Walltime).Equal(until)
}

// reserve finds where and when a job could start, given what is running.
//
// It simulates the running jobs on each candidate node finishing in the order
// their time limits expire, and returns the earliest moment enough capacity
// exists. A job with no time limit is never assumed to finish, so a node
// occupied by one offers no prediction at all.
func reserve(j *job.Job, nodes []NodeState, taken map[string]time.Time,
	now time.Time) (name, label string, at time.Time, ok bool) {

	if now.IsZero() {
		return "", "", time.Time{}, false
	}
	best, bestLabel := "", ""
	var bestAt time.Time
	for _, n := range nodes {
		if n.Drained || !n.Online {
			continue
		}
		// A machine already held for an earlier job is not offered again.
		// Without this, every blocked job reserves the machine that frees up
		// soonest, so the second reservation onward is a no-op and the rest
		// of the cluster stays idle behind a queue that thinks it is held.
		//
		// Properly, a node could be reserved twice for two different times;
		// doing that needs a time-indexed capacity map, and skipping errs
		// toward holding less idle capacity, which is the safer direction.
		if _, held := taken[n.Name]; held {
			continue
		}
		// Capacity and constraints have to be satisfiable on an empty node,
		// or waiting for this one is pointless.
		empty := n
		empty.UsedCPUs, empty.UsedMem, empty.UsedGPUs, empty.UsedGPUMem = 0, 0, 0, 0
		if ok, _ := Fits(j, empty); !ok {
			continue
		}
		at, ok := freesUpAt(j, n, now)
		if !ok {
			continue
		}
		if best == "" || at.Before(bestAt) {
			best, bestLabel, bestAt = n.Name, n.shown(), at
		}
	}
	if best == "" {
		return "", "", time.Time{}, false
	}
	return best, bestLabel, bestAt, true
}

// freesUpAt is when a node will have room for j.
func freesUpAt(j *job.Job, n NodeState, now time.Time) (time.Time, bool) {
	if ok, _ := Fits(j, n); ok {
		return now, true
	}
	// Ordered by when each finishes; anything without a time limit is
	// excluded, because assuming a finish time it does not have is how a
	// reservation becomes a promise that gets broken.
	ends := make([]RunningJob, 0, len(n.Running))
	for _, r := range n.Running {
		if !r.EndsAt.IsZero() {
			ends = append(ends, r)
		}
	}
	sort.Slice(ends, func(a, b int) bool { return ends[a].EndsAt.Before(ends[b].EndsAt) })

	sim := n
	for _, r := range ends {
		cpus := r.CPUs
		if cpus <= 0 {
			cpus = 1
		}
		sim.UsedCPUs -= cpus
		sim.UsedMem -= r.MemBytes
		sim.UsedGPUs -= r.GPUs
		if sim.UsedCPUs < 0 {
			sim.UsedCPUs = 0
		}
		if sim.UsedMem < 0 {
			sim.UsedMem = 0
		}
		if sim.UsedGPUs < 0 {
			sim.UsedGPUs = 0
		}
		if ok, _ := Fits(j, sim); ok {
			at := r.EndsAt
			if at.Before(now) {
				at = now
			}
			return at, true
		}
	}
	return time.Time{}, false
}

// reservationReason explains why a job that fits is not being started.
func reservationReason(node string, until time.Time, j *job.Job) string {
	if j.Spec.Limits.Walltime <= 0 {
		return fmt.Sprintf("%s is held for a higher-priority job from %s, and this "+
			"job has no --time limit so it cannot be shown to finish first",
			node, until.Format("15:04:05"))
	}
	return fmt.Sprintf("%s is held for a higher-priority job from %s, and this job "+
		"would still be running then", node, until.Format("15:04:05"))
}
