package ctl

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/davidwu/shome/internal/fairshare"
	"github.com/davidwu/shome/internal/job"
	"github.com/davidwu/shome/internal/sched"
	"github.com/davidwu/shome/internal/store"
)

// Priority: turning job history into a queue order.
//
// Off by default. A cluster with one user wants submission order and nothing
// else, and an admin who has not asked for fair-share should not discover it
// reordering their queue.

// usageWindow is how far back job history is read.
//
// Several half-lives: beyond that a contribution is multiplied by so small a
// number that reading it costs more than it changes. Expressed in half-lives
// rather than as a fixed span so that an admin who sets a long half-life
// still gets the memory they asked for.
const usageHalfLives = 6

// shareTTL is how long a computed set of shares is reused.
//
// Short: the numbers move as jobs run, and a scheduling pass that reorders
// the queue on a minute-old picture of usage would oscillate. Long enough
// that a busy scheduler is not re-reading the job table every pass.
const shareTTL = 5 * time.Second

// shareCache holds the last computed standings.
type shareCache struct {
	mu    sync.Mutex
	at    time.Time
	cfg   fairshare.Config
	value []fairshare.Share
}

// PriorityConfig is the policy in force.
func (c *Controller) PriorityConfig() fairshare.Config {
	return c.priority.Get(c.log)
}

// SetPriorityConfig replaces priority.yaml.
func (c *Controller) SetPriorityConfig(cfg fairshare.Config) error {
	return fairshare.Save(c.root, cfg)
}

// Shares returns every account's fair-share standing.
//
// This is what `sshare` prints and what the scheduler orders by, so
// there is one computation rather than two that could disagree.
func (c *Controller) Shares(ctx context.Context) ([]fairshare.Share, error) {
	cfg := c.PriorityConfig()
	now := c.now()

	c.shares.mu.Lock()
	if !c.shares.at.IsZero() && now.Sub(c.shares.at) < shareTTL &&
		c.shares.cfg.HalfLife == cfg.HalfLife &&
		c.shares.cfg.DefaultShares == cfg.DefaultShares &&
		sameShares(c.shares.cfg.Shares, cfg.Shares) {
		out := c.shares.value
		c.shares.mu.Unlock()
		return out, nil
	}
	c.shares.mu.Unlock()

	usage, err := c.usageFor(ctx, cfg, now)
	if err != nil {
		return nil, err
	}
	users, err := c.store.Users(ctx)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(users))
	for _, u := range users {
		names = append(names, u.Name)
	}
	out := cfg.SharesFor(names, usage)

	c.shares.mu.Lock()
	c.shares.at, c.shares.cfg, c.shares.value = now, cfg, out
	c.shares.mu.Unlock()
	return out, nil
}

// sameShares reports whether two share maps are equal, so an admin changing
// one account's entitlement invalidates the cache immediately.
func sameShares(a, b map[string]float64) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// usageFor computes each account's decayed usage from the job history.
func (c *Controller) usageFor(ctx context.Context, cfg fairshare.Config,
	now time.Time) ([]fairshare.Usage, error) {

	window := time.Duration(usageHalfLives) * cfg.HalfLife
	rows, err := c.store.UsageSince(ctx, now.Add(-window))
	if err != nil {
		return nil, err
	}
	byUser := map[string]float64{}
	for _, r := range rows {
		end := r.End
		if end.IsZero() || end.After(now) {
			// Still running: it has consumed up to now, and no further.
			end = now
		}
		if !end.After(r.Start) {
			continue
		}
		// Decayed from the midpoint of the job rather than its end. Using the
		// end would credit a week-long job that finished this morning as if
		// all of it happened today; using the start would discount its most
		// recent hours. The midpoint is the cheap, defensible middle.
		mid := r.Start.Add(end.Sub(r.Start) / 2)
		cost := cfg.Cost(r.CPUs, r.GPUs, r.MemBytes, end.Sub(r.Start))
		byUser[r.User] += cost * cfg.Decay(now.Sub(mid))
	}
	out := make([]fairshare.Usage, 0, len(byUser))
	for u, v := range byUser {
		out = append(out, fairshare.Usage{User: u, ResourceSeconds: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].User < out[j].User })
	return out, nil
}

// orderByPriority sorts a round of pending jobs, and records each job's score
// so `squeue` and "why is this pending" can report it.
//
// Returns the jobs unchanged when priority is off, which is what makes this
// safe to call unconditionally.
func (c *Controller) orderByPriority(ctx context.Context, jobs []*job.Job) []*job.Job {
	cfg := c.PriorityConfig()
	// Scored even when there is only one job to order. Skipping that case
	// would save a cached lookup and make a user with a single pending job
	// unable to see its priority at all, which reads as the feature being
	// broken.
	if !cfg.Enabled || len(jobs) == 0 {
		c.setScores(nil)
		return jobs
	}
	shares, err := c.Shares(ctx)
	if err != nil {
		// Never block work on a bookkeeping failure: fall back to submission
		// order, which is what the cluster did before priority existed.
		c.log.Error("fair-share standings unavailable; using submission order", "err", err)
		c.setScores(nil)
		return jobs
	}

	now := c.now()
	in := make([]fairshare.Job, 0, len(jobs))
	byID := make(map[int64]*job.Job, len(jobs))
	for _, j := range jobs {
		byID[j.ID] = j
		in = append(in, fairshare.Job{
			ID: j.ID, User: j.Spec.User, SubmitAt: j.SubmitAt,
			CPUs: j.Spec.Limits.CPUs, GPUs: j.Spec.Limits.GPUs,
			MemBytes: j.Spec.Limits.MemBytes,
		})
	}
	ordered, scores := fairshare.NewScorer(cfg, shares, now).Order(in)
	c.setScores(scores)

	out := make([]*job.Job, 0, len(ordered))
	for _, o := range ordered {
		out = append(out, byID[o.ID])
	}
	return out
}

// setScores publishes the last round's scores for display.
func (c *Controller) setScores(s map[int64]fairshare.Score) {
	c.scoreMu.Lock()
	c.scores = s
	c.scoreMu.Unlock()
}

// ScoreOf returns a pending job's priority from the last scheduling pass.
//
// From the last pass rather than recomputed on demand: what a user wants to
// know is the number the scheduler actually used, and a freshly computed one
// would differ from it and explain nothing.
func (c *Controller) ScoreOf(id int64) (fairshare.Score, bool) {
	c.scoreMu.Lock()
	defer c.scoreMu.Unlock()
	s, ok := c.scores[id]
	return s, ok
}

// AllScores returns the last pass's scores.
func (c *Controller) AllScores() map[int64]fairshare.Score {
	c.scoreMu.Lock()
	defer c.scoreMu.Unlock()
	out := make(map[int64]fairshare.Score, len(c.scores))
	for k, v := range c.scores {
		out[k] = v
	}
	return out
}

// schedOptions turns the policy into scheduler options.
func (c *Controller) schedOptions() sched.Options {
	cfg := c.PriorityConfig()
	if !cfg.Backfill {
		return sched.Options{}
	}
	return sched.Options{Backfill: true, Depth: cfg.BackfillDepth, Now: c.now()}
}

// runningOn tells the scheduler what is on each node and when it should end,
// which is what backfill needs to work out when a gap opens.
func (c *Controller) runningOn(ctx context.Context) map[string][]sched.RunningJob {
	out := map[string][]sched.RunningJob{}
	running, err := c.store.List(ctx, "", true)
	if err != nil {
		c.log.Error("running jobs for backfill", "err", err)
		return out
	}
	for _, j := range running {
		if j.State != job.Running || j.Node == "" {
			continue
		}
		r := sched.RunningJob{
			ID: j.ID, CPUs: j.Spec.Limits.CPUs,
			MemBytes: j.Spec.Limits.MemBytes, GPUs: j.Spec.Limits.GPUs,
		}
		// Only a job with a time limit has a predictable end. Leaving EndsAt
		// zero is how the scheduler is told it cannot make a promise about
		// this one, and that distinction is what keeps backfill honest.
		if j.Spec.Limits.Walltime > 0 && !j.StartAt.IsZero() {
			r.EndsAt = j.StartAt.Add(j.Spec.Limits.Walltime)
		}
		out[j.Node] = append(out[j.Node], r)
	}
	return out
}

// SetUserShares changes one account's entitlement.
func (c *Controller) SetUserShares(ctx context.Context, user string, shares float64) error {
	if _, err := c.store.UserByName(ctx, user); err != nil {
		return err
	}
	cfg := c.PriorityConfig()
	if cfg.Shares == nil {
		cfg.Shares = map[string]float64{}
	}
	if shares <= 0 {
		delete(cfg.Shares, user)
	} else {
		cfg.Shares[user] = shares
	}
	if err := c.SetPriorityConfig(cfg); err != nil {
		return err
	}
	// The standings depend on shares, so they are stale the moment one
	// changes. Dropping the cache means the next reader sees the new number
	// rather than one up to five seconds old.
	c.shares.mu.Lock()
	c.shares.at = time.Time{}
	c.shares.mu.Unlock()
	c.store.Event(ctx, 0, "shares", user, c.now())
	return nil
}

// ShareOf returns one account's standing, for a user asking about themselves.
func (c *Controller) ShareOf(ctx context.Context, user string) (fairshare.Share, error) {
	all, err := c.Shares(ctx)
	if err != nil {
		return fairshare.Share{}, err
	}
	for _, s := range all {
		if s.User == user {
			return s, nil
		}
	}
	// An account with no jobs and no row yet still has an entitlement.
	cfg := c.PriorityConfig()
	return fairshare.Share{User: user, Shares: cfg.SharesOf(user), Factor: 1}, nil
}

var _ = store.User{}
