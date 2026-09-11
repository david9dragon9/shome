package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/davidwu/shome/internal/agentapi"
	"github.com/davidwu/shome/internal/owner"
	"github.com/davidwu/shome/internal/platform"
)

// A status file the machine's owner can read without a controller.
//
// The owner's perspective has to work when the cluster does not. Their CLI
// talks to a controller over a unix socket on the controller's own machine,
// so on any other node there is nothing to ask -- and "what is shome doing to
// my computer right now" is a question that matters most when something is
// wrong, which is exactly when the controller may be unreachable.
//
// So the agent writes what it knows to a file beside its state, every
// heartbeat. Cheap, needs no credentials, and readable by the person whose
// machine it is.

// StatusFile is where the agent publishes its local view.
func StatusFile(root string) string { return filepath.Join(root, "node-status.json") }

// LocalJob is one job running on this machine, as its owner sees it.
//
// Deliberately not the full job record: an owner is entitled to know what is
// running on their hardware and what it is consuming, which is not the same
// as being entitled to read other people's job scripts or arguments.
type LocalJob struct {
	ID         int64   `json:"id"`
	Name       string  `json:"name"`
	User       string  `json:"user"`
	CPUs       int     `json:"cpus"`
	MemMiB     int64   `json:"mem_mib"`
	GPUs       int     `json:"gpus"`
	LiveMiB    int64   `json:"live_mib"`
	PeakMiB    int64   `json:"peak_mib"`
	Procs      int     `json:"procs"`
	ElapsedSec float64 `json:"elapsed_sec"`
	Throttled  bool    `json:"throttled"`
}

// LocalStatus is the whole local view.
type LocalStatus struct {
	At   time.Time `json:"at"`
	Node string    `json:"node"`
	// Controller is what this agent connects to, empty on a controller.
	Controller string `json:"controller,omitempty"`
	// Connected reports whether the last heartbeat succeeded, so an owner can
	// tell "nothing is running" from "nothing can be reported".
	Connected bool   `json:"connected"`
	LastError string `json:"last_error,omitempty"`

	OwnerAction string `json:"owner_action"`
	OwnerReason string `json:"owner_reason,omitempty"`
	Paused      bool   `json:"paused"`
	PauseReason string `json:"pause_reason,omitempty"`
	DiskRefusal string `json:"disk_refusal,omitempty"`

	// Advertised is what the cluster is allowed to use, after the owner's
	// caps. Shown next to the machine's real capacity so the difference the
	// policy makes is visible.
	AdvertisedCPUs   int      `json:"advertised_cpus"`
	AdvertisedMemMiB int64    `json:"advertised_mem_mib"`
	AdvertisedGPUs   int      `json:"advertised_gpus"`
	TotalCPUs        int      `json:"total_cpus"`
	TotalMemMiB      int64    `json:"total_mem_mib"`
	TotalGPUs        int      `json:"total_gpus"`
	Tier             string   `json:"tier"`
	Limitations      []string `json:"limitations,omitempty"`

	Telemetry platform.Telemetry `json:"telemetry"`
	Jobs      []LocalJob         `json:"jobs"`
	// Completed counts jobs this agent has finished since it started, so an
	// idle machine can still show it has been doing something.
	Completed int `json:"completed_since_start"`
}

// WriteStatus publishes the local view. Best effort: a machine that cannot
// write its own state directory has larger problems, and failing to publish
// must never interfere with running work.
func (a *Agent) WriteStatus(root string, s LocalStatus) {
	if root == "" {
		// Never relative: filepath.Join("", name) is a bare filename, so an
		// unset root would publish this into whatever directory the process
		// happened to start in. That is never what was meant, and it is how
		// a stray node-status.json ended up committed to the repository.
		return
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return
	}
	tmp := StatusFile(root) + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return
	}
	// Rename so a reader never sees a half-written file.
	os.Rename(tmp, StatusFile(root))
}

// ReadStatus loads what an agent published, for the owner's tools.
func ReadStatus(root string) (LocalStatus, error) {
	var s LocalStatus
	b, err := os.ReadFile(StatusFile(root))
	if err != nil {
		return s, err
	}
	err = json.Unmarshal(b, &s)
	return s, err
}

// Completed is how many jobs have finished since this agent started.
func (a *Agent) Completed() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.completed
}

// LocalJobs describes what is running here, for the status file.
func (a *Agent) LocalJobs() []LocalJob {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	out := make([]LocalJob, 0, len(a.run))
	for id, r := range a.run {
		out = append(out, LocalJob{
			ID: id, Name: r.spec.Name, User: r.spec.User,
			CPUs: r.spec.Limits.CPUs, MemMiB: r.spec.Limits.MemBytes >> 20,
			GPUs:    r.spec.Limits.GPUs,
			LiveMiB: r.curMem >> 20, PeakMiB: r.peak >> 20,
			Procs: r.curProcs, ElapsedSec: now.Sub(r.started).Seconds(),
			Throttled: r.throttled,
		})
	}
	return out
}

// buildStatus assembles the local view from what the agent already knows.
func (a *Agent) buildStatus(hb agentapi.Heartbeat, caps platform.Capabilities,
	connected bool, lastErr string, completed int) LocalStatus {

	s := LocalStatus{
		At: a.now(), Node: a.Node, Connected: connected, LastError: lastErr,
		OwnerAction: hb.OwnerAction, OwnerReason: hb.OwnerReason,
		AdvertisedCPUs: hb.Caps.CPUs, AdvertisedMemMiB: hb.Caps.MemBytes >> 20,
		AdvertisedGPUs: hb.Caps.GPUs,
		TotalCPUs:      caps.CPUs, TotalMemMiB: caps.MemBytes >> 20, TotalGPUs: caps.GPUs,
		Tier: caps.Tier, Limitations: caps.Lost,
		Telemetry: hb.Telemetry, Jobs: a.LocalJobs(), Completed: completed,
	}
	if a.Owner != nil {
		s.Paused, s.PauseReason = a.Owner.Paused()
		s.DiskRefusal = a.Owner.CheckDisk(diskState(hb.Telemetry))
	}
	return s
}

var _ = owner.DiskState{}
