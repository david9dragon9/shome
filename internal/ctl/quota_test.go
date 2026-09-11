package ctl

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/davidwu/shome/internal/fairshare"
	"github.com/davidwu/shome/internal/job"
	"github.com/davidwu/shome/internal/platform"
	"github.com/davidwu/shome/internal/qos"
	"github.com/davidwu/shome/internal/store"
)

func quotaFixture(t *testing.T) (*Controller, *store.Store, context.Context, time.Time) {
	t.Helper()
	c, st := dashFixture(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	c.nowFn = func() time.Time { return now }
	for _, u := range []string{"alice", "bob"} {
		if _, err := st.CreateUser(ctx, u, store.RoleUser, 0, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.CreateUser(ctx, "root", store.RoleAdmin, 0, now); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertNode(ctx, "n1", "127.0.0.1:1", platform.Capabilities{
		OS: "test", Arch: "test", CPUs: 8, MemBytes: 64 << 30,
	}, now); err != nil {
		t.Fatal(err)
	}
	return c, st, ctx, now
}

func getQuota(t *testing.T, c *Controller, caller *store.User, who string) QuotaView {
	t.Helper()
	a := &API{c: c}
	url := "/quota"
	if who != "" {
		url += "?user=" + who
	}
	w := httptest.NewRecorder()
	a.quotaFor(w, httptest.NewRequest("GET", url, nil), caller)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var v QuotaView
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// The whole point: one answer covering compute, storage and standing.
func TestQuotaCoversEverythingAUserAsksAbout(t *testing.T) {
	c, st, ctx, now := quotaFixture(t)
	if err := c.SetUserQoS(ctx, "alice", qos.Limits{
		MaxRunningCPUs: iptr(16), MaxDiskMB: i64ptr(1024),
	}); err != nil {
		t.Fatal(err)
	}
	// Something running, and something stored.
	j, err := st.Submit(ctx, job.Spec{Name: "j", User: "alice", Script: "x",
		ArrayTaskID: -1, Limits: job.Limits{CPUs: 4, MemBytes: 2 << 30}}, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.MarkRunning(ctx, j.ID, "n1", "", now); err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceNodeFiles(ctx, "n1", "alice",
		[]store.UserFile{{Path: "f", Size: 5 << 20, MTime: now}},
		store.NodeUsage{Bytes: 5 << 20, Files: 1}, now); err != nil {
		t.Fatal(err)
	}

	v := getQuota(t, c, &store.User{Name: "alice", Role: store.RoleUser}, "")
	if v.User != "alice" {
		t.Fatalf("user = %q", v.User)
	}

	byName := map[string]QuotaLine{}
	for _, l := range v.Compute {
		byName[l.Name] = l
	}
	if got := byName["cpu cores in use"]; got.Used != 4 || got.Limit != 16 {
		t.Errorf("cpus = %d of %d, want 4 of 16", got.Used, got.Limit)
	}
	if got := byName["jobs running"]; got.Used != 1 {
		t.Errorf("running jobs = %d, want 1", got.Used)
	}
	if got := byName["memory in use"]; got.Used != 2048 {
		t.Errorf("memory = %d MiB, want 2048", got.Used)
	}
	if v.Storage.TotalBytes != 5<<20 {
		t.Errorf("storage = %d, want %d", v.Storage.TotalBytes, 5<<20)
	}
	if v.Storage.LimitBytes != 1024<<20 {
		t.Errorf("storage limit = %d, want %d", v.Storage.LimitBytes, 1024<<20)
	}
	if len(v.PerJob) == 0 {
		t.Error("no per-job ceilings reported")
	}
}

// Zero and unlimited are different answers, and collapsing them would tell
// somebody they had unlimited GPUs when they had been denied every one.
func TestQuotaKeepsZeroAndUnlimitedApart(t *testing.T) {
	c, _, ctx, _ := quotaFixture(t)
	if err := c.SetUserQoS(ctx, "alice", qos.Limits{
		MaxRunningGPUs: iptr(0), MaxRunningCPUs: iptr(qos.Unlimited),
	}); err != nil {
		t.Fatal(err)
	}
	v := getQuota(t, c, &store.User{Name: "alice", Role: store.RoleUser}, "")
	byName := map[string]QuotaLine{}
	for _, l := range v.Compute {
		byName[l.Name] = l
	}
	if got := byName["gpus in use"].Limit; got != 0 {
		t.Errorf("a zero GPU limit came through as %d", got)
	}
	if got := byName["cpu cores in use"].Limit; got >= 0 {
		t.Errorf("an unlimited CPU limit came through as %d, want negative", got)
	}
	// And the percentage is meaningless for both, so it is not invented.
	if p := byName["cpu cores in use"].Percent(); p >= 0 {
		t.Errorf("unlimited reported %v%% full", p)
	}
	if p := byName["gpus in use"].Percent(); p >= 0 {
		t.Errorf("a zero limit reported %v%% full", p)
	}
}

func TestQuotaPercent(t *testing.T) {
	for _, c := range []struct {
		used, limit int64
		want        float64
	}{
		{0, 100, 0}, {50, 100, 50}, {100, 100, 100},
		{150, 100, 150}, // over is reported honestly rather than clamped
		{5, -1, -1},     // unlimited
		{5, 0, -1},      // none allowed
	} {
		got := QuotaLine{Used: c.used, Limit: c.limit}.Percent()
		if got != c.want {
			t.Errorf("Percent(%d of %d) = %v, want %v", c.used, c.limit, got, c.want)
		}
	}
}

// Somebody else's usage is not a plain user's business.
func TestQuotaIsScopedToTheCaller(t *testing.T) {
	c, _, _, _ := quotaFixture(t)
	v := getQuota(t, c, &store.User{Name: "alice", Role: store.RoleUser}, "bob")
	if v.User != "alice" {
		t.Errorf("a plain user asking about bob got %q; it must be their own", v.User)
	}
	// An admin may.
	v = getQuota(t, c, &store.User{Name: "root", Role: store.RoleAdmin}, "bob")
	if v.User != "bob" {
		t.Errorf("an admin asking about bob got %q", v.User)
	}
}

func TestQuotaRejectsAnUnknownAccount(t *testing.T) {
	c, _, _, _ := quotaFixture(t)
	a := &API{c: c}
	w := httptest.NewRecorder()
	a.quotaFor(w, httptest.NewRequest("GET", "/quota?user=ghost", nil),
		&store.User{Name: "root", Role: store.RoleAdmin})
	if w.Code == http.StatusOK {
		t.Error("an account that does not exist reported a quota")
	}
}

// Fair-share is only shown when the cluster acts on it; a number nothing acts
// on invites the reader to act on it.
func TestQuotaShowsStandingOnlyWhenPriorityIsOn(t *testing.T) {
	c, _, _, _ := quotaFixture(t)
	v := getQuota(t, c, &store.User{Name: "alice", Role: store.RoleUser}, "")
	if v.PriorityOn || v.Share != nil {
		t.Error("standing was reported with priority off")
	}
	if v.PriorityHint == "" {
		t.Error("nothing explained that the queue is in submission order")
	}

	enablePriority(t, c, nil)
	v = getQuota(t, c, &store.User{Name: "alice", Role: store.RoleUser}, "")
	if !v.PriorityOn || v.Share == nil {
		t.Fatal("standing was not reported with priority on")
	}
	if v.PriorityHint == "" {
		t.Error("the factor was reported with nothing saying what it means")
	}
}

// The hint has to say something different at each end of the range, or it is
// decoration.
func TestShareHintTracksTheFactor(t *testing.T) {
	seen := map[string]bool{}
	for _, f := range []float64{1.0, 0.9, 0.5, 0.2, 0.01} {
		h := shareHint(fairShareAt(f))
		if h == "" {
			t.Fatalf("no hint at factor %v", f)
		}
		seen[h] = true
	}
	if len(seen) < 4 {
		t.Errorf("only %d distinct hints across the range; it does not track "+
			"the factor", len(seen))
	}
}

// fairShareAt builds a standing with a given factor, for the hint test.
func fairShareAt(f float64) fairshare.Share {
	return fairshare.Share{User: "alice", Shares: 1, RawUsage: 1, Factor: f}
}
