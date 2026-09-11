package ctl

import (
	"context"
	"net/http"
	"sort"
	"time"

	"github.com/davidwu/shome/internal/job"
	"github.com/davidwu/shome/internal/platform"
	"github.com/davidwu/shome/internal/store"
)

// The dashboard endpoint. One request returns everything a live view needs:
// cluster totals, per-node capacity and utilisation, the queue, and recent
// events.
//
// One request rather than five because a dashboard polling every second
// against five endpoints produces five inconsistent snapshots -- a node that
// appears idle next to a job that claims to be running on it -- and because
// the CLI has to make each of those calls over a unix socket in series.

// StaleAfter is how long a telemetry reading stays believable.
//
// Just over two heartbeat intervals: long enough to ride out one missed
// sample, short enough that a machine which has genuinely gone away stops
// showing plausible numbers.
const StaleAfter = 25 * time.Second

// NodeDash is one machine's full state: what it has, what is allocated, and
// what it is actually doing.
type NodeDash struct {
	// Note is the admin's reminder about this machine, if they left one.
	Note string `json:"note,omitempty"`

	NodeInfo

	// Live utilisation. Stale is set when the reading is too old to trust, in
	// which case the numbers are the last known ones and should be shown as
	// such rather than as current.
	Telemetry platform.Telemetry `json:"telemetry"`
	HasTelem  bool               `json:"has_telemetry"`
	Stale     bool               `json:"stale"`
	History   []Sample           `json:"history,omitempty"`

	// GPU inventory, which NodeInfo reduces to a count.
	GPUKind       string `json:"gpu_kind,omitempty"`
	GPUName       string `json:"gpu_name,omitempty"`
	GPUMemMiB     int64  `json:"gpu_mem_mib"`
	UsedGPUMemMiB int64  `json:"used_gpu_mem_mib"`

	RunningJobs int `json:"running_jobs"`
}

// ClusterTotals is the whole cluster in one line.
type ClusterTotals struct {
	Nodes      int `json:"nodes"`
	NodesUp    int `json:"nodes_up"`
	NodesDrain int `json:"nodes_drain"`
	NodesDown  int `json:"nodes_down"`

	CPUs       int   `json:"cpus"`
	UsedCPUs   int   `json:"used_cpus"`
	MemMiB     int64 `json:"mem_mib"`
	UsedMemMiB int64 `json:"used_mem_mib"`
	GPUs       int   `json:"gpus"`
	GPUMemMiB  int64 `json:"gpu_mem_mib"`

	// Measured, as opposed to allocated. Averaged over nodes that reported.
	CPUPercent float64 `json:"cpu_percent"`
	MemPercent float64 `json:"mem_percent"`
	Reporting  int     `json:"reporting"`

	Running   int `json:"running"`
	Pending   int `json:"pending"`
	Held      int `json:"held"`
	Completed int `json:"completed"`
	Failed    int `json:"failed"`
}

// Dashboard is everything a live view needs, consistent as of one instant.
type Dashboard struct {
	// Cluster is what this cluster is called, so the console's title and the
	// rename prompt read the same value everything else does.
	Cluster string `json:"cluster,omitempty"`

	At     time.Time     `json:"at"`
	Totals ClusterTotals `json:"totals"`
	Nodes  []NodeDash    `json:"nodes"`
	Jobs   []JobView     `json:"jobs"`
	Recent []JobView     `json:"recent"`
	Events []EventView   `json:"events,omitempty"`
	Alerts []string      `json:"alerts,omitempty"`
}

// EventView is an audit entry as the dashboard shows it.
type EventView struct {
	At     string `json:"at"`
	JobID  int64  `json:"job_id,omitempty"`
	Kind   string `json:"kind"`
	Detail string `json:"detail,omitempty"`
}

// BuildDashboard assembles the whole live view.
func (c *Controller) BuildDashboard(ctx context.Context, history bool) (*Dashboard, error) {
	now := c.now()
	d := &Dashboard{At: now, Cluster: c.ClusterName()}

	infos, err := c.NodeInfos(ctx)
	if err != nil {
		return nil, err
	}
	nodes, err := c.store.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	cfg := c.ClusterConfig()
	caps := map[string]platform.Capabilities{}
	for _, n := range nodes {
		caps[n.Name] = n.Caps
	}

	var cpuSum, memSum float64
	for _, info := range infos {
		nd := NodeDash{NodeInfo: info}
		cp := caps[info.Name]
		nd.GPUKind, nd.GPUName = cp.GPUKind, cp.GPUName
		nd.GPUMemMiB = cp.GPUMemBytes >> 20

		if t, ok := c.metrics.Latest(info.Name); ok {
			nd.Telemetry, nd.HasTelem = t, true
			nd.Stale = c.metrics.Stale(info.Name, StaleAfter, now)
			if t.KnownGPUMem() {
				nd.UsedGPUMemMiB = t.GPUMemUsedBytes >> 20
			} else {
				nd.UsedGPUMemMiB = -1
			}
			// Only count nodes whose reading is both present and current, so
			// a departed machine cannot drag the cluster average toward idle.
			if !nd.Stale {
				if t.KnownCPU() {
					cpuSum += t.CPUPercent
					d.Totals.Reporting++
				}
				if mp := t.MemPercent(); mp >= 0 {
					memSum += mp
				}
			}
		} else {
			nd.UsedGPUMemMiB = -1
		}
		nd.RunningJobs = len(c.metrics.LiveJobs(info.Name))
		if history {
			nd.History = c.metrics.History(info.Name)
		}
		// Named for display, last: everything above keys off the certificate
		// identity, so relabelling earlier would silently break the metrics
		// and capability lookups.
		nd.Name = cfg.LabelOf(info.Name)
		nd.Note = cfg.NoteOf(info.Name)
		d.Nodes = append(d.Nodes, nd)

		d.Totals.Nodes++
		switch info.State {
		case string(store.NodeUp):
			d.Totals.NodesUp++
		case string(store.NodeDrain):
			d.Totals.NodesDrain++
		default:
			d.Totals.NodesDown++
		}
		// Capacity of unreachable machines is not capacity. Counting a DOWN
		// node's cores in the cluster total is how a dashboard reports 40
		// idle CPUs on a cluster that cannot run anything.
		if info.State != string(store.NodeDown) {
			d.Totals.CPUs += info.CPUs
			d.Totals.MemMiB += info.MemMiB
			d.Totals.GPUs += info.GPUs
			d.Totals.GPUMemMiB += nd.GPUMemMiB
		}
		d.Totals.UsedCPUs += info.UsedCPUs
		d.Totals.UsedMemMiB += info.UsedMemMiB
	}
	if d.Totals.Reporting > 0 {
		d.Totals.CPUPercent = cpuSum / float64(d.Totals.Reporting)
		d.Totals.MemPercent = memSum / float64(d.Totals.Reporting)
	} else {
		d.Totals.CPUPercent = platform.Unknown
		d.Totals.MemPercent = platform.Unknown
	}

	active, err := c.store.List(ctx, "", true)
	if err != nil {
		return nil, err
	}
	for _, j := range active {
		v := view(j, now)
		// Attach live usage so the queue shows what a job is doing now, not
		// only what it asked for.
		if l, ok := c.metrics.JobUsage(j.ID); ok {
			v.LiveMiB = l.MemBytes >> 20
			v.NProcs = l.NProcs
			v.Throttled = l.Throttled
		} else {
			v.LiveMiB = -1
		}
		d.Jobs = append(d.Jobs, v)
		switch {
		case j.State == job.Running:
			d.Totals.Running++
		case j.Held:
			d.Totals.Held++
		default:
			d.Totals.Pending++
		}
	}
	sort.Slice(d.Jobs, func(i, k int) bool { return d.Jobs[i].ID > d.Jobs[k].ID })

	recent, err := c.store.RecentFinished(ctx, 15)
	if err == nil {
		for _, j := range recent {
			d.Recent = append(d.Recent, view(j, now))
			if j.State == job.Completed {
				d.Totals.Completed++
			} else if j.State.Terminal() {
				d.Totals.Failed++
			}
		}
	}

	d.Alerts = c.alerts(d)
	return d, nil
}

// alerts surfaces things a person should look at, so a dashboard is not just
// numbers someone has to interpret.
func (c *Controller) alerts(d *Dashboard) []string {
	var out []string
	for _, n := range d.Nodes {
		switch {
		case n.State == string(store.NodeDown):
			out = append(out, n.Name+" is down"+reasonSuffix(n.Reason))
		case n.State == string(store.NodeDrain):
			out = append(out, n.Name+" is draining"+reasonSuffix(n.Reason))
		case n.HasTelem && n.Stale:
			out = append(out, n.Name+" has stopped reporting utilisation")
		}
		if n.HasTelem && !n.Stale {
			switch n.Telemetry.Thermal {
			case "serious", "critical":
				out = append(out, n.Name+" is thermally throttled ("+n.Telemetry.Thermal+")")
			}
			if n.Telemetry.MemoryPressure == "critical" {
				out = append(out, n.Name+" is under critical memory pressure")
			}
			if n.Telemetry.OnBattery {
				out = append(out, n.Name+" is on battery")
			}
		}
	}
	for _, j := range d.Jobs {
		if j.Reason != "" && j.State == string(job.Pending) && !j.Held {
			continue // ordinary "waiting for resources"; not an alert
		}
		if j.Throttled {
			out = append(out, "job "+j.Label+" is throttled for the machine's owner")
		}
	}
	return out
}

func reasonSuffix(r string) string {
	if r == "" {
		return ""
	}
	return ": " + r
}

// dashboard serves the whole live view in one request.
func (a *API) dashboard(w http.ResponseWriter, r *http.Request, caller *store.User) {
	d, err := a.c.BuildDashboard(r.Context(), r.URL.Query().Get("history") != "0")
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	// A non-admin sees only their own work, exactly as squeue does. The node
	// view is shared: capacity is not private, and hiding it would make the
	// dashboard useless for deciding whether to submit anything.
	if caller.Role == store.RoleUser {
		d.Jobs = filterOwn(d.Jobs, caller.Name)
		d.Recent = filterOwn(d.Recent, caller.Name)
		d.Events = nil
	}
	writeJSON(w, http.StatusOK, d)
}

func filterOwn(in []JobView, user string) []JobView {
	out := make([]JobView, 0, len(in))
	for _, j := range in {
		if j.User == user {
			out = append(out, j)
		}
	}
	return out
}

// nodeHistory returns one node's sample history, for drawing a larger chart
// than the dashboard's inline sparkline.
func (a *API) nodeHistory(w http.ResponseWriter, r *http.Request, caller *store.User) {
	name := r.PathValue("name")
	h := a.c.Metrics().History(name)
	if h == nil {
		// An empty history and an unknown node are different situations, and
		// a chart that silently draws nothing for a typo is hard to debug.
		if _, err := a.c.Store().NodeByName(r.Context(), name); err != nil {
			fail(w, http.StatusNotFound, "no such node "+name)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"node": name, "samples": h})
}
