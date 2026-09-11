// Package fairshare scores pending jobs, so the queue can be ordered by
// something other than who submitted first.
//
// # Why this exists
//
// Strict FIFO is fine for one person. With several, it means whoever submits
// a hundred jobs at nine in the morning owns the cluster until lunchtime, and
// the only recourse is asking them to stop. Fair-share fixes that by making
// priority depend on how much of the cluster an account has *already* used:
// heavy recent use lowers your priority, idleness raises it, and nobody has
// to negotiate.
//
// # The shape, and why it is Slurm's
//
// The scoring is deliberately the meaningful subset of Slurm's multifactor
// priority, using Slurm's own formula for the fair-share factor:
//
//	F = 2^(-U/S)
//
// where U is the account's share of recent cluster usage and S is its share
// of the allocated shares. Using exactly your entitlement gives F = 0.5;
// using none gives F = 1; using far more decays toward 0. People who have
// run a real cluster already know this curve, and `shome share` reports the
// same columns `sshare` does.
//
// Factors shome does not have are absent rather than faked: there are no
// partitions and no QoS tiers to weight. Age, fair-share and job size are
// real here, and are what an admin can weight.
//
// # Usage is derived, not accumulated
//
// Usage is computed from the job history each time it is needed, with an
// exponential decay applied per job, rather than kept as a running counter.
// A counter drifts -- every crash, every requeue, every manual database edit
// is a chance to lose sync -- and a fair-share number that has quietly
// diverged from what actually ran is worse than none, because it is
// unarguable. Deriving it is self-correcting, and a home cluster's job count
// is small enough that a short cache makes it cheap. The same reasoning as
// disk accounting, which walks the tree rather than counting writes.
package fairshare

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// Defaults chosen to be defensible rather than tuned: fair-share dominates,
// age breaks ties and eventually rescues a starved job, and size is off by
// default because whether big jobs should be favoured depends on the cluster.
const (
	DefaultHalfLife      = 7 * 24 * time.Hour
	DefaultMaxAge        = 7 * 24 * time.Hour
	DefaultShares        = 1.0
	DefaultWeightFair    = 1.0
	DefaultWeightAge     = 0.2
	DefaultWeightSize    = 0.0
	DefaultGPUWeight     = 25.0
	DefaultMemGBWeight   = 0.25
	DefaultBackfillDepth = 1
)

// Weights say what matters and how much.
//
// Relative, not absolute: they are normalised by their own sum, so doubling
// every weight changes nothing and an admin can think in ratios.
type Weights struct {
	// FairShare rewards accounts that have used less than their entitlement.
	FairShare float64 `yaml:"fairshare" json:"fairshare"`
	// Age rewards jobs that have waited. This is what stops a heavy user's
	// job from waiting forever behind a stream of light users' work.
	Age float64 `yaml:"age" json:"age"`
	// Size weights job size. Zero by default: whether a cluster should
	// prefer big jobs (so they are not starved by a stream of small ones) or
	// small jobs (so latency stays low) is a local decision.
	Size float64 `yaml:"size" json:"size"`
}

// Sum is the normalising denominator.
func (w Weights) Sum() float64 { return w.FairShare + w.Age + w.Size }

// ResourceWeights convert a job's resources into one usage number.
//
// A GPU-hour and a CPU-hour are not comparable, and pretending they are makes
// fair-share meaningless on a cluster where the GPUs are the scarce thing. So
// usage is a weighted sum, and the weights are the admin's to set: they
// encode what is actually scarce here.
type ResourceWeights struct {
	// CPU is fixed at 1: it is the unit the others are expressed in.
	GPU   float64 `yaml:"gpu" json:"gpu"`
	MemGB float64 `yaml:"mem_gb" json:"mem_gb"`
}

// Config is the priority policy.
type Config struct {
	// Enabled turns priority ordering on. Off means strict submission order,
	// which is what shome did before this existed and remains the right
	// default for a cluster with one user.
	Enabled bool `yaml:"enabled" json:"enabled"`

	// Backfill lets a lower-priority job run in a gap, provided it will
	// finish before the job it would otherwise delay.
	Backfill bool `yaml:"backfill" json:"backfill"`

	// BackfillDepth is how many blocked jobs get a reservation. Beyond that,
	// jobs simply wait. One is Slurm's practical default and is enough to
	// stop the largest job in the queue being starved.
	BackfillDepth int `yaml:"backfill_depth" json:"backfill_depth"`

	Weights  Weights         `yaml:"weights" json:"weights"`
	Resource ResourceWeights `yaml:"resource_weights" json:"resource_weights"`

	// HalfLife is how long it takes for past usage to count half as much.
	// Short means the cluster forgives quickly; long means it has a memory.
	HalfLife time.Duration `yaml:"half_life" json:"half_life"`

	// MaxAge is the wait at which the age factor reaches its maximum.
	MaxAge time.Duration `yaml:"max_age" json:"max_age"`

	// FavorSmall makes the size factor prefer smaller jobs. Only meaningful
	// when Weights.Size is non-zero.
	FavorSmall bool `yaml:"favor_small" json:"favor_small"`

	// DefaultShares is what an account gets when it is not listed below.
	DefaultShares float64 `yaml:"default_shares" json:"default_shares"`

	// Shares is the entitlement per account. Two accounts with shares 2 and
	// 1 are entitled to two thirds and one third of the cluster over time.
	Shares map[string]float64 `yaml:"shares,omitempty" json:"shares,omitempty"`
}

// Default is the policy a cluster gets before anyone configures one.
func Default() Config {
	return Config{
		Enabled:       false,
		Backfill:      true,
		BackfillDepth: DefaultBackfillDepth,
		Weights: Weights{
			FairShare: DefaultWeightFair,
			Age:       DefaultWeightAge,
			Size:      DefaultWeightSize,
		},
		Resource: ResourceWeights{
			GPU:   DefaultGPUWeight,
			MemGB: DefaultMemGBWeight,
		},
		HalfLife:      DefaultHalfLife,
		MaxAge:        DefaultMaxAge,
		DefaultShares: DefaultShares,
	}
}

// WithDefaults fills in anything a hand-edited file left out.
//
// A file that sets only `enabled: true` must produce a working policy, not a
// cluster where every weight is zero and every job scores the same.
func (c Config) WithDefaults() Config {
	d := Default()
	if c.Weights.Sum() <= 0 {
		c.Weights = d.Weights
	}
	if c.Resource.GPU <= 0 {
		c.Resource.GPU = d.Resource.GPU
	}
	if c.Resource.MemGB <= 0 {
		c.Resource.MemGB = d.Resource.MemGB
	}
	if c.HalfLife <= 0 {
		c.HalfLife = d.HalfLife
	}
	if c.MaxAge <= 0 {
		c.MaxAge = d.MaxAge
	}
	if c.DefaultShares <= 0 {
		c.DefaultShares = d.DefaultShares
	}
	if c.BackfillDepth <= 0 {
		c.BackfillDepth = d.BackfillDepth
	}
	return c
}

// SharesOf is an account's entitlement.
func (c Config) SharesOf(user string) float64 {
	if s, ok := c.Shares[user]; ok && s > 0 {
		return s
	}
	if c.DefaultShares > 0 {
		return c.DefaultShares
	}
	return DefaultShares
}

// Usage is one account's decayed resource consumption.
type Usage struct {
	User string
	// ResourceSeconds is the decayed weighted total: cpu-seconds plus
	// GPU and memory converted by the resource weights.
	ResourceSeconds float64
}

// Cost converts a job's resources and duration into usage.
//
// Exported because it is the definition of "how much of the cluster did you
// use", and both the accounting and the tests should be reading the same one.
func (c Config) Cost(cpus, gpus int, memBytes int64, dur time.Duration) float64 {
	if dur <= 0 {
		return 0
	}
	if cpus <= 0 {
		cpus = 1
	}
	units := float64(cpus) +
		float64(gpus)*c.Resource.GPU +
		(float64(memBytes)/(1<<30))*c.Resource.MemGB
	return units * dur.Seconds()
}

// Decay is the weight a contribution from `age` ago still carries.
func (c Config) Decay(age time.Duration) float64 {
	if age <= 0 {
		return 1
	}
	if c.HalfLife <= 0 {
		return 1
	}
	return math.Pow(0.5, age.Seconds()/c.HalfLife.Seconds())
}

// Share is what an account's standing looks like, and what `shome share`
// prints. The column names follow sshare, because the concept is the same and
// inventing new words for it would help nobody.
type Share struct {
	User string `json:"user"`
	// Shares is the account's entitlement.
	Shares float64 `json:"shares"`
	// NormShares is its entitlement as a fraction of all entitlements.
	NormShares float64 `json:"norm_shares"`
	// RawUsage is decayed weighted resource-seconds.
	RawUsage float64 `json:"raw_usage"`
	// NormUsage is its usage as a fraction of the cluster's.
	NormUsage float64 `json:"norm_usage"`
	// Factor is 2^(-NormUsage/NormShares): 1 means unused, 0.5 means using
	// exactly the entitlement, below that means over.
	Factor float64 `json:"factor"`
}

// Shares computes every account's standing.
//
// Accounts with no usage are included, because "you have used nothing and
// your priority is high" is exactly what a new user needs to be told.
func (c Config) SharesFor(users []string, usage []Usage) []Share {
	c = c.WithDefaults()
	byUser := map[string]float64{}
	for _, u := range usage {
		byUser[u.User] += u.ResourceSeconds
	}
	// Anyone with usage but no account row still counts toward the total: a
	// deleted account's recent usage did consume the cluster, and ignoring it
	// would inflate everyone else's share of what is left.
	seen := map[string]bool{}
	all := make([]string, 0, len(users)+len(byUser))
	for _, u := range users {
		if !seen[u] {
			seen[u] = true
			all = append(all, u)
		}
	}
	for u := range byUser {
		if !seen[u] {
			seen[u] = true
			all = append(all, u)
		}
	}
	sort.Strings(all)

	var totalShares, totalUsage float64
	for _, u := range all {
		totalShares += c.SharesOf(u)
		totalUsage += byUser[u]
	}

	out := make([]Share, 0, len(all))
	for _, u := range all {
		s := Share{User: u, Shares: c.SharesOf(u), RawUsage: byUser[u]}
		if totalShares > 0 {
			s.NormShares = s.Shares / totalShares
		}
		if totalUsage > 0 {
			s.NormUsage = s.RawUsage / totalUsage
		}
		s.Factor = FairFactor(s.NormUsage, s.NormShares)
		out = append(out, s)
	}
	return out
}

// FairFactor is Slurm's fair-share curve: 2^(-U/S).
//
// The edge cases are the interesting part. No usage at all gives 1, the
// highest priority, which is what a new account should get. Usage with no
// shares gives 0: an account entitled to nothing that is nevertheless
// consuming should sort last, not divide by zero.
func FairFactor(normUsage, normShares float64) float64 {
	switch {
	case normUsage <= 0:
		return 1
	case normShares <= 0:
		return 0
	}
	return math.Pow(2, -normUsage/normShares)
}

// Score is a job's priority and the factors it came from.
//
// The breakdown is carried rather than discarded because "why is my job
// behind that one" has to be answerable, and a single opaque number cannot
// answer it.
type Score struct {
	JobID    int64   `json:"job_id"`
	Priority float64 `json:"priority"`
	Fair     float64 `json:"fair"`
	Age      float64 `json:"age"`
	Size     float64 `json:"size"`
}

// Job is what scoring needs to know about a pending job.
type Job struct {
	ID       int64
	User     string
	SubmitAt time.Time
	CPUs     int
	GPUs     int
	MemBytes int64
}

// Scorer holds the precomputed per-account factors for one scheduling pass.
type Scorer struct {
	cfg   Config
	fair  map[string]float64
	now   time.Time
	maxSz float64
}

// NewScorer prepares to score a round of jobs.
func NewScorer(cfg Config, shares []Share, now time.Time) *Scorer {
	cfg = cfg.WithDefaults()
	fair := make(map[string]float64, len(shares))
	for _, s := range shares {
		fair[s.User] = s.Factor
	}
	return &Scorer{cfg: cfg, fair: fair, now: now}
}

// Prepare records the largest job in the round, which the size factor is
// relative to. Called with the whole round before scoring any of it.
func (s *Scorer) Prepare(jobs []Job) {
	s.maxSz = 0
	for _, j := range jobs {
		if c := s.cfg.Cost(j.CPUs, j.GPUs, j.MemBytes, time.Second); c > s.maxSz {
			s.maxSz = c
		}
	}
}

// Score computes one job's priority.
func (s *Scorer) Score(j Job) Score {
	w := s.cfg.Weights
	sum := w.Sum()
	if sum <= 0 {
		sum = 1
	}

	// An account nobody has scored -- a brand new one -- has used nothing,
	// so it gets the top fair-share factor rather than zero. Defaulting to
	// zero would silently put every new user last.
	fair, ok := s.fair[j.User]
	if !ok {
		fair = 1
	}

	age := 0.0
	if !j.SubmitAt.IsZero() && s.cfg.MaxAge > 0 {
		waited := s.now.Sub(j.SubmitAt)
		if waited > 0 {
			age = waited.Seconds() / s.cfg.MaxAge.Seconds()
			if age > 1 {
				age = 1
			}
		}
	}

	size := 0.0
	if s.maxSz > 0 {
		size = s.cfg.Cost(j.CPUs, j.GPUs, j.MemBytes, time.Second) / s.maxSz
		if s.cfg.FavorSmall {
			size = 1 - size
		}
	}

	p := (w.FairShare*fair + w.Age*age + w.Size*size) / sum
	return Score{JobID: j.ID, Priority: p, Fair: fair, Age: age, Size: size}
}

// Order sorts jobs by priority, highest first.
//
// Ties break on submission order and then on id, so the result is total and
// deterministic. A scheduler whose output depends on map iteration is one
// whose bugs cannot be reproduced.
func (s *Scorer) Order(jobs []Job) ([]Job, map[int64]Score) {
	s.Prepare(jobs)
	scores := make(map[int64]Score, len(jobs))
	for _, j := range jobs {
		scores[j.ID] = s.Score(j)
	}
	out := make([]Job, len(jobs))
	copy(out, jobs)
	sort.SliceStable(out, func(a, b int) bool {
		pa, pb := scores[out[a].ID].Priority, scores[out[b].ID].Priority
		if pa != pb {
			return pa > pb
		}
		if !out[a].SubmitAt.Equal(out[b].SubmitAt) {
			return out[a].SubmitAt.Before(out[b].SubmitAt)
		}
		return out[a].ID < out[b].ID
	})
	return out, scores
}

// Describe explains a score in words, for "why is my job pending".
func (s Score) Describe() string {
	return fmt.Sprintf("priority %.4f (fair-share %.2f, age %.2f, size %.2f)",
		s.Priority, s.Fair, s.Age, s.Size)
}
