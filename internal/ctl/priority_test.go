package ctl

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/davidwu/shome/internal/fairshare"
	"github.com/davidwu/shome/internal/job"
	"github.com/davidwu/shome/internal/platform"
	"github.com/davidwu/shome/internal/store"
)

// prioFixture is a controller with two accounts and one node.
func prioFixture(t *testing.T, cpus int) (*Controller, *store.Store, context.Context, time.Time) {
	t.Helper()
	c, st := dashFixture(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	c.nowFn = func() time.Time { return now }

	for _, u := range []string{"heavy", "light"} {
		if _, err := st.CreateUser(ctx, u, store.RoleUser, 0, now); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.UpsertNode(ctx, "n1", "127.0.0.1:1", platform.Capabilities{
		OS: "test", Arch: "test", CPUs: cpus, MemBytes: 64 << 30,
	}, now); err != nil {
		t.Fatal(err)
	}
	return c, st, ctx, now
}

// ranJob records a finished job, which is what fair-share reads.
func ranJob(t *testing.T, st *store.Store, user string, cpus int,
	start time.Time, dur time.Duration) {

	t.Helper()
	ctx := context.Background()
	j, err := st.Submit(ctx, job.Spec{
		Name: "past", User: user, Script: "x", ArrayTaskID: -1,
		Limits: job.Limits{CPUs: cpus},
	}, start)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.MarkRunning(ctx, j.ID, "n1", "", start); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkFinished(ctx, j.ID, job.Completed, 0, "", 0, start.Add(dur)); err != nil {
		t.Fatal(err)
	}
}

func pend(t *testing.T, st *store.Store, user string, cpus int,
	wall time.Duration, at time.Time) int64 {

	t.Helper()
	j, err := st.Submit(context.Background(), job.Spec{
		Name: "p", User: user, Script: "x", ArrayTaskID: -1,
		Limits: job.Limits{CPUs: cpus, Walltime: wall},
	}, at)
	if err != nil {
		t.Fatal(err)
	}
	return j.ID
}

func enablePriority(t *testing.T, c *Controller, mutate func(*fairshare.Config)) {
	t.Helper()
	cfg := fairshare.Default().WithDefaults()
	cfg.Enabled = true
	if mutate != nil {
		mutate(&cfg)
	}
	if err := c.SetPriorityConfig(cfg); err != nil {
		t.Fatal(err)
	}
}

// Usage has to come from the job history, or the whole feature rests on
// nothing. A heavy account's factor must be below a light one's.
func TestSharesComeFromRealJobHistory(t *testing.T) {
	c, st, ctx, now := prioFixture(t, 8)
	enablePriority(t, c, nil)

	// A day of 4 CPUs for heavy, ten minutes of 1 CPU for light.
	ranJob(t, st, "heavy", 4, now.Add(-25*time.Hour), 24*time.Hour)
	ranJob(t, st, "light", 1, now.Add(-time.Hour), 10*time.Minute)

	shares, err := c.Shares(ctx)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]fairshare.Share{}
	for _, s := range shares {
		m[s.User] = s
	}
	if m["heavy"].RawUsage <= m["light"].RawUsage {
		t.Fatalf("heavy usage %v not above light %v",
			m["heavy"].RawUsage, m["light"].RawUsage)
	}
	if m["heavy"].Factor >= m["light"].Factor {
		t.Errorf("heavy factor %v not below light %v -- fair-share would do nothing",
			m["heavy"].Factor, m["light"].Factor)
	}
	// Both accounts appear even though only two ran anything.
	if len(shares) != 2 {
		t.Errorf("got %d accounts, want 2", len(shares))
	}
}

// A running job's usage must accrue as it runs. Counting only finished jobs
// would let someone launch a week of work and show zero usage all week.
func TestRunningJobsCountTowardUsage(t *testing.T) {
	c, st, ctx, now := prioFixture(t, 8)
	enablePriority(t, c, nil)

	j := pend(t, st, "heavy", 4, 0, now.Add(-2*time.Hour))
	if err := st.MarkRunning(ctx, j, "n1", "", now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	shares, err := c.Shares(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range shares {
		if s.User == "heavy" && s.RawUsage <= 0 {
			t.Error("a job that has been running for two hours counted as no usage")
		}
	}
}

// The headline behaviour: with priority on, the account that has used less
// goes first even though it submitted second.
func TestPriorityReordersTheQueue(t *testing.T) {
	c, st, ctx, now := prioFixture(t, 1) // one CPU: only one job can run
	enablePriority(t, c, nil)
	ranJob(t, st, "heavy", 8, now.Add(-2*time.Hour), time.Hour)

	first := pend(t, st, "heavy", 1, time.Hour, now.Add(-time.Minute))
	second := pend(t, st, "light", 1, time.Hour, now)

	c.schedule(ctx)

	h, _ := st.Get(ctx, first)
	l, _ := st.Get(ctx, second)
	if l.State != job.Running {
		t.Errorf("the light account's job is %s, not running -- priority did "+
			"not reorder the queue (reason: %q)", l.State, l.Reason)
	}
	if h.State == job.Running {
		t.Error("the heavy account's job ran despite lower priority")
	}
}

// And with priority off, the same setup runs in submission order. This is the
// control: without it, the test above could pass for the wrong reason.
func TestSubmissionOrderWhenPriorityIsOff(t *testing.T) {
	c, st, ctx, now := prioFixture(t, 1)
	// Deliberately not enabling priority.
	ranJob(t, st, "heavy", 8, now.Add(-2*time.Hour), time.Hour)

	first := pend(t, st, "heavy", 1, time.Hour, now.Add(-time.Minute))
	second := pend(t, st, "light", 1, time.Hour, now)

	c.schedule(ctx)

	h, _ := st.Get(ctx, first)
	l, _ := st.Get(ctx, second)
	if h.State != job.Running {
		t.Errorf("with priority off the first-submitted job should run, got %s (%q)",
			h.State, h.Reason)
	}
	if l.State == job.Running {
		t.Error("the second job ran with priority off and one CPU free")
	}
}

// Backfill: a small short job must get past a big blocked one.
func TestBackfillLetsASmallJobPastABlockedOne(t *testing.T) {
	c, st, ctx, now := prioFixture(t, 4)
	enablePriority(t, c, func(cfg *fairshare.Config) { cfg.Backfill = true })

	big := pend(t, st, "light", 16, time.Hour, now.Add(-time.Minute)) // never fits
	small := pend(t, st, "light", 1, time.Minute, now)

	c.schedule(ctx)

	b, _ := st.Get(ctx, big)
	s, _ := st.Get(ctx, small)
	if b.State == job.Running {
		t.Fatal("a job needing 16 CPUs ran on a 4-CPU node")
	}
	if s.State != job.Running {
		t.Errorf("the small job did not backfill: %s (%q)", s.State, s.Reason)
	}
	if b.Reason == "" {
		t.Error("the blocked job was given no reason")
	}
}

// With backfill off, the same setup blocks -- the control for the above.
func TestWithoutBackfillTheQueueBlocks(t *testing.T) {
	c, st, ctx, now := prioFixture(t, 4)
	enablePriority(t, c, func(cfg *fairshare.Config) { cfg.Backfill = false })

	pend(t, st, "light", 16, time.Hour, now.Add(-time.Minute))
	small := pend(t, st, "light", 1, time.Minute, now)

	c.schedule(ctx)

	s, _ := st.Get(ctx, small)
	if s.State == job.Running {
		t.Error("a job ran past a blocked one with backfill off")
	}
}

// Scores are published for display, so "why is my job behind that one" can be
// answered with the number the scheduler actually used.
func TestScoresArePublishedForDisplay(t *testing.T) {
	c, st, ctx, now := prioFixture(t, 1)
	enablePriority(t, c, nil)
	ranJob(t, st, "heavy", 8, now.Add(-time.Hour), time.Hour)

	a := pend(t, st, "heavy", 1, time.Hour, now)
	b := pend(t, st, "light", 1, time.Hour, now)
	c.schedule(ctx)

	sa, oka := c.ScoreOf(a)
	sb, okb := c.ScoreOf(b)
	if !oka || !okb {
		t.Fatalf("scores missing: %v %v", oka, okb)
	}
	if sb.Priority <= sa.Priority {
		t.Errorf("light priority %v not above heavy %v", sb.Priority, sa.Priority)
	}
	if sa.Describe() == "" {
		t.Error("Describe produced nothing to show a user")
	}
	// With priority off, nothing is published -- showing a stale score would
	// imply the scheduler was using it.
	if err := c.SetPriorityConfig(fairshare.Default()); err != nil {
		t.Fatal(err)
	}
	c.schedule(ctx)
	if _, ok := c.ScoreOf(a); ok {
		t.Error("scores are still published with priority off")
	}
}

// Shares are an entitlement: doubling one account's shares should raise its
// factor for the same usage.
func TestSharesChangeTheFactor(t *testing.T) {
	c, st, ctx, now := prioFixture(t, 8)
	enablePriority(t, c, nil)
	ranJob(t, st, "heavy", 4, now.Add(-time.Hour), time.Hour)
	ranJob(t, st, "light", 4, now.Add(-time.Hour), time.Hour)

	before, err := c.ShareOf(ctx, "heavy")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SetUserShares(ctx, "heavy", 4); err != nil {
		t.Fatal(err)
	}
	after, err := c.ShareOf(ctx, "heavy")
	if err != nil {
		t.Fatal(err)
	}
	if after.Factor <= before.Factor {
		t.Errorf("factor %v did not rise from %v after quadrupling shares",
			after.Factor, before.Factor)
	}
	// Setting shares must take effect at once, not after the cache expires.
	if after.Shares != 4 {
		t.Errorf("shares = %v, want 4", after.Shares)
	}
	// Zero restores the default.
	if err := c.SetUserShares(ctx, "heavy", 0); err != nil {
		t.Fatal(err)
	}
	back, _ := c.ShareOf(ctx, "heavy")
	if back.Shares != 1 {
		t.Errorf("shares = %v after clearing, want the default 1", back.Shares)
	}
}

func TestSharesRefusesAnUnknownAccount(t *testing.T) {
	c, _, ctx, _ := prioFixture(t, 1)
	if err := c.SetUserShares(ctx, "nobody", 2); err == nil {
		t.Error("shares were set for an account that does not exist")
	}
}

// A broken policy file must not silently turn priority off across the
// cluster: an admin who enabled fair-share would find FIFO back with no
// indication why.
func TestBrokenPolicyFileKeepsWhatWasInForce(t *testing.T) {
	c, _, _, _ := prioFixture(t, 1)
	enablePriority(t, c, nil)
	if !c.PriorityConfig().Enabled {
		t.Fatal("priority did not turn on")
	}
	if err := os.WriteFile(fairshare.File(c.root),
		[]byte("weights: [not, a, map\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !c.PriorityConfig().Enabled {
		t.Error("a syntax error turned priority off, so a typo would silently " +
			"revert the whole cluster to submission order")
	}
}
