package ctl

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/davidwu/shome/internal/agentapi"
	"github.com/davidwu/shome/internal/job"
	"github.com/davidwu/shome/internal/platform"
	"github.com/davidwu/shome/internal/store"
)

func dashFixture(t *testing.T) (*Controller, *store.Store) {
	t.Helper()
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "shome.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return New(st, root, slog.New(slog.NewTextHandler(io.Discard, nil))), st
}

func caps(cpus int, memMiB int64, gpus int) platform.Capabilities {
	return platform.Capabilities{
		OS: "linux", Arch: "amd64", Tier: "full",
		CPUs: cpus, MemBytes: memMiB << 20, GPUs: gpus,
		MemLimit: "hard", CPULimit: "hard",
	}
}

func TestDashboardTotals(t *testing.T) {
	c, st := dashFixture(t)
	ctx := context.Background()
	now := time.Now()
	st.UpsertNode(ctx, "a", "", caps(8, 16384, 1), now)
	st.UpsertNode(ctx, "b", "", caps(4, 8192, 0), now)

	d, err := c.BuildDashboard(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if d.Totals.Nodes != 2 || d.Totals.NodesUp != 2 {
		t.Errorf("nodes = %d up %d, want 2/2", d.Totals.Nodes, d.Totals.NodesUp)
	}
	if d.Totals.CPUs != 12 {
		t.Errorf("CPUs = %d, want 12", d.Totals.CPUs)
	}
	if d.Totals.MemMiB != 24576 {
		t.Errorf("MemMiB = %d, want 24576", d.Totals.MemMiB)
	}
	// Nothing has reported utilisation, so the measured figure must be
	// unknown rather than a confident zero.
	if d.Totals.CPUPercent >= 0 {
		t.Errorf("CPUPercent = %v with no telemetry; want unknown", d.Totals.CPUPercent)
	}
}

// A DOWN machine's cores are not cluster capacity. Counting them is how a
// dashboard cheerfully reports idle CPUs on a cluster that cannot run a thing.
func TestDownNodeIsNotCapacity(t *testing.T) {
	c, st := dashFixture(t)
	ctx := context.Background()
	now := time.Now()
	st.UpsertNode(ctx, "a", "", caps(8, 16384, 1), now)
	st.UpsertNode(ctx, "gone", "", caps(64, 65536, 4), now)
	st.SetNodeState(ctx, "gone", store.NodeDown, "missed heartbeats")

	d, err := c.BuildDashboard(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if d.Totals.CPUs != 8 {
		t.Errorf("CPUs = %d, want 8: a DOWN node's capacity was counted", d.Totals.CPUs)
	}
	if d.Totals.GPUs != 1 {
		t.Errorf("GPUs = %d, want 1", d.Totals.GPUs)
	}
	if d.Totals.NodesDown != 1 {
		t.Errorf("NodesDown = %d, want 1", d.Totals.NodesDown)
	}
}

func TestDashboardAveragesOnlyFreshNodes(t *testing.T) {
	c, st := dashFixture(t)
	ctx := context.Background()
	now := time.Now()
	st.UpsertNode(ctx, "fresh", "", caps(4, 4096, 0), now)
	st.UpsertNode(ctx, "quiet", "", caps(4, 4096, 0), now)

	c.metrics.Record("fresh", tel(now, 80, 2<<30, 4<<30), nil)
	// An old reading: the node stopped reporting some time ago.
	c.metrics.Record("quiet", tel(now.Add(-10*time.Minute), 0, 0, 4<<30), nil)

	d, err := c.BuildDashboard(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if d.Totals.Reporting != 1 {
		t.Fatalf("Reporting = %d, want 1", d.Totals.Reporting)
	}
	// Averaging in the stale zero would report 40% and hide a busy machine.
	if d.Totals.CPUPercent != 80 {
		t.Errorf("CPUPercent = %v, want 80 (a stale reading was averaged in)", d.Totals.CPUPercent)
	}
	for _, n := range d.Nodes {
		if n.Name == "quiet" && !n.Stale {
			t.Error("the ten-minute-old node was not marked stale")
		}
	}
}

func TestDashboardAttachesLiveJobUsage(t *testing.T) {
	c, st := dashFixture(t)
	ctx := context.Background()
	now := time.Now()
	st.UpsertNode(ctx, "a", "", caps(4, 4096, 0), now)
	j0, err := st.Submit(ctx, job.Spec{Name: "j", User: "ada", Script: "true",
		Limits: job.Limits{CPUs: 1}}, now)
	if err != nil {
		t.Fatal(err)
	}
	id := j0.ID
	if err := st.MarkRunning(ctx, id, "a", "/tmp/s", now); err != nil {
		t.Fatal(err)
	}
	c.metrics.Record("a", tel(now, 10, 1<<30, 4<<30),
		[]agentapi.JobLive{{ID: id, MemBytes: 512 << 20, NProcs: 3, Throttled: true}})

	d, err := c.BuildDashboard(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Jobs) != 1 {
		t.Fatalf("got %d jobs, want 1", len(d.Jobs))
	}
	j := d.Jobs[0]
	if j.LiveMiB != 512 {
		t.Errorf("LiveMiB = %d, want 512", j.LiveMiB)
	}
	if j.NProcs != 3 || !j.Throttled {
		t.Errorf("NProcs = %d, Throttled = %v", j.NProcs, j.Throttled)
	}
}

// A job whose node has not reported must read as unknown, not as zero memory.
func TestJobWithoutTelemetryReadsUnknown(t *testing.T) {
	c, st := dashFixture(t)
	ctx := context.Background()
	now := time.Now()
	st.UpsertNode(ctx, "a", "", caps(4, 4096, 0), now)
	j0, _ := st.Submit(ctx, job.Spec{Name: "j", User: "u", Script: "true"}, now)
	st.MarkRunning(ctx, j0.ID, "a", "/tmp/s", now)

	d, _ := c.BuildDashboard(ctx, false)
	if d.Jobs[0].LiveMiB != -1 {
		t.Errorf("LiveMiB = %d, want -1 for a job with no reading", d.Jobs[0].LiveMiB)
	}
}

func TestAlertsSurfaceRealProblems(t *testing.T) {
	c, st := dashFixture(t)
	ctx := context.Background()
	now := time.Now()
	st.UpsertNode(ctx, "hot", "", caps(4, 4096, 0), now)
	st.UpsertNode(ctx, "dead", "", caps(4, 4096, 0), now)
	st.SetNodeState(ctx, "dead", store.NodeDown, "missed heartbeats")

	tt := tel(now, 50, 1<<30, 4<<30)
	tt.Thermal = "critical"
	tt.OnBattery = true
	c.metrics.Record("hot", tt, nil)

	d, _ := c.BuildDashboard(ctx, false)
	joined := ""
	for _, a := range d.Alerts {
		joined += a + "\n"
	}
	for _, want := range []string{"dead is down", "thermally throttled", "on battery"} {
		if !contains(joined, want) {
			t.Errorf("alerts missing %q; got:\n%s", want, joined)
		}
	}
}

// A plain user must not see other people's jobs in the dashboard, exactly as
// they cannot in squeue. Capacity stays visible; that is not private.
func TestDashboardFiltersJobsForPlainUsers(t *testing.T) {
	c, st := dashFixture(t)
	ctx := context.Background()
	now := time.Now()
	st.UpsertNode(ctx, "a", "", caps(4, 4096, 0), now)
	mine, _ := st.Submit(ctx, job.Spec{Name: "mine", User: "alice", Script: "true"}, now)
	st.Submit(ctx, job.Spec{Name: "theirs", User: "bob", Script: "true"}, now)

	d, _ := c.BuildDashboard(ctx, false)
	if len(d.Jobs) != 2 {
		t.Fatalf("admin view has %d jobs, want 2", len(d.Jobs))
	}
	got := filterOwn(d.Jobs, "alice")
	if len(got) != 1 || got[0].ID != mine.ID {
		t.Fatalf("alice sees %d job(s), want only her own", len(got))
	}
}

func TestDashboardHistoryIsOptional(t *testing.T) {
	c, st := dashFixture(t)
	ctx := context.Background()
	now := time.Now()
	st.UpsertNode(ctx, "a", "", caps(4, 4096, 0), now)
	c.metrics.Record("a", tel(now, 10, 1, 2), nil)

	without, _ := c.BuildDashboard(ctx, false)
	if len(without.Nodes[0].History) != 0 {
		t.Error("history was included when it was not asked for")
	}
	with, _ := c.BuildDashboard(ctx, true)
	if len(with.Nodes[0].History) == 0 {
		t.Error("history was requested but not returned")
	}
}

func contains(hay, needle string) bool {
	return len(hay) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(hay); i++ {
			if hay[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}

// Listing keys must be scoped: one user learning which machines another logs
// in from is a privacy leak, and the fingerprints are an inventory of somebody
// else's computers.
func TestKeyListingIsScopedForPlainUsers(t *testing.T) {
	c, st := dashFixture(t)
	ctx := context.Background()
	now := time.Now()
	for _, n := range []string{"alice", "bob"} {
		if _, err := st.CreateUser(ctx, n, store.RoleUser, 0, now); err != nil {
			t.Fatal(err)
		}
		if err := st.AddUserKey(ctx, n, "SHA256:fp-"+n, "ssh-ed25519 AAAA "+n, n+"@box", now); err != nil {
			t.Fatal(err)
		}
	}
	a := &API{c: c}
	for _, tc := range []struct {
		caller *store.User
		query  string
		want   int
	}{
		{&store.User{Name: "alice", Role: store.RoleUser}, "", 1},
		// Even asking for someone else's explicitly.
		{&store.User{Name: "alice", Role: store.RoleUser}, "?user=bob", 1},
		{&store.User{Name: "root", Role: store.RoleAdmin}, "", 2},
	} {
		r := httptest.NewRequest("GET", "/keys"+tc.query, nil)
		w := httptest.NewRecorder()
		a.listUserKeys(w, r, tc.caller)
		var got struct {
			Keys  []map[string]any `json:"keys"`
			Codes []map[string]any `json:"codes"`
		}
		json.NewDecoder(w.Body).Decode(&got)
		if len(got.Keys) != tc.want {
			t.Errorf("%s%s saw %d keys, want %d", tc.caller.Name, tc.query, len(got.Keys), tc.want)
		}
		if tc.caller.Role == store.RoleUser {
			// Both halves must be scoped: another user's pending codes are as
			// much their business as their fingerprints are.
			for _, k := range append(got.Keys, got.Codes...) {
				if k["user"] != tc.caller.Name {
					t.Errorf("%s saw an entry belonging to %v", tc.caller.Name, k["user"])
				}
			}
		}
	}
}
