package fairshare

import (
	"math"
	"testing"
	"time"
)

func close2(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// Slurm's curve, and the three points on it people reason about: using
// nothing is the top of the range, using exactly your entitlement is the
// middle, using double decays toward the bottom.
func TestFairFactorMatchesTheSlurmCurve(t *testing.T) {
	for _, c := range []struct{ usage, shares, want float64 }{
		{0.0, 0.5, 1.0},  // used nothing
		{0.5, 0.5, 0.5},  // used exactly the entitlement
		{1.0, 0.5, 0.25}, // used double
		{0.25, 0.5, math.Pow(2, -0.5)},
	} {
		if got := FairFactor(c.usage, c.shares); !close2(got, c.want) {
			t.Errorf("FairFactor(%v, %v) = %v, want %v", c.usage, c.shares, got, c.want)
		}
	}
	// An account entitled to nothing but consuming must sort last, not
	// divide by zero.
	if got := FairFactor(0.5, 0); got != 0 {
		t.Errorf("FairFactor with no shares = %v, want 0", got)
	}
	// And one entitled to nothing that has used nothing is not penalised.
	if got := FairFactor(0, 0); got != 1 {
		t.Errorf("FairFactor(0,0) = %v, want 1", got)
	}
}

// The reason the feature exists: someone who has been hammering the cluster
// must sort behind someone who has not.
func TestHeavyUserSortsBehindIdleUser(t *testing.T) {
	cfg := Default().WithDefaults()
	cfg.Enabled = true
	now := time.Unix(1_700_000_000, 0)

	shares := cfg.SharesFor([]string{"heavy", "idle"}, []Usage{
		{User: "heavy", ResourceSeconds: 100_000},
		{User: "idle", ResourceSeconds: 0},
	})
	sc := NewScorer(cfg, shares, now)

	// Both submitted at the same moment, so only fair-share separates them.
	jobs := []Job{
		{ID: 1, User: "heavy", SubmitAt: now, CPUs: 1},
		{ID: 2, User: "idle", SubmitAt: now, CPUs: 1},
	}
	ordered, scores := sc.Order(jobs)
	if ordered[0].ID != 2 {
		t.Fatalf("order = %d then %d; the idle account should come first",
			ordered[0].ID, ordered[1].ID)
	}
	if scores[2].Priority <= scores[1].Priority {
		t.Errorf("idle priority %v not above heavy %v",
			scores[2].Priority, scores[1].Priority)
	}
}

// The other half: age must eventually rescue a starved job, or a heavy user's
// work waits forever behind a stream of light users'.
func TestAgeEventuallyOutweighsFairShare(t *testing.T) {
	cfg := Default().WithDefaults()
	cfg.Weights = Weights{FairShare: 1, Age: 1}
	cfg.MaxAge = time.Hour
	now := time.Unix(1_700_000_000, 0)

	shares := cfg.SharesFor([]string{"heavy", "idle"}, []Usage{
		{User: "heavy", ResourceSeconds: 1_000_000},
	})
	sc := NewScorer(cfg, shares, now)

	// The heavy user's job has waited the full age window; the idle user has
	// just submitted.
	old := Job{ID: 1, User: "heavy", SubmitAt: now.Add(-time.Hour), CPUs: 1}
	fresh := Job{ID: 2, User: "idle", SubmitAt: now, CPUs: 1}
	ordered, _ := sc.Order([]Job{fresh, old})
	if ordered[0].ID != 1 {
		t.Error("a fully aged job never overtakes a fresh one, so a heavy " +
			"user's work would starve")
	}

	// And with no wait, fair-share still wins -- age must not dominate from
	// the start.
	ordered, _ = sc.Order([]Job{
		{ID: 1, User: "heavy", SubmitAt: now, CPUs: 1},
		{ID: 2, User: "idle", SubmitAt: now, CPUs: 1},
	})
	if ordered[0].ID != 2 {
		t.Error("fair-share lost to age at zero wait")
	}
}

func TestDecayHalvesAtTheHalfLife(t *testing.T) {
	cfg := Config{HalfLife: 24 * time.Hour}.WithDefaults()
	cfg.HalfLife = 24 * time.Hour
	for _, c := range []struct {
		age  time.Duration
		want float64
	}{
		{0, 1},
		{24 * time.Hour, 0.5},
		{48 * time.Hour, 0.25},
		{-time.Hour, 1}, // a clock that went backwards must not amplify usage
	} {
		if got := cfg.Decay(c.age); !close2(got, c.want) {
			t.Errorf("Decay(%v) = %v, want %v", c.age, got, c.want)
		}
	}
}

// A GPU-hour and a CPU-hour are not comparable, and treating them as equal
// makes fair-share meaningless where the GPUs are the scarce thing.
func TestCostWeightsScarceResources(t *testing.T) {
	cfg := Default().WithDefaults()
	cpuOnly := cfg.Cost(1, 0, 0, time.Hour)
	withGPU := cfg.Cost(1, 1, 0, time.Hour)
	if withGPU <= cpuOnly {
		t.Errorf("a GPU adds nothing to cost: %v vs %v", withGPU, cpuOnly)
	}
	if !close2(withGPU-cpuOnly, DefaultGPUWeight*3600) {
		t.Errorf("GPU cost = %v, want the weight times the duration", withGPU-cpuOnly)
	}
	// Memory counts, in GB.
	withMem := cfg.Cost(1, 0, 4<<30, time.Hour)
	if !close2(withMem-cpuOnly, 4*DefaultMemGBWeight*3600) {
		t.Errorf("memory cost = %v", withMem-cpuOnly)
	}
	// A job with no duration has cost nothing yet.
	if got := cfg.Cost(8, 4, 1<<30, 0); got != 0 {
		t.Errorf("Cost with zero duration = %v, want 0", got)
	}
	// CPUs default to one, so a job that never declared any is not free.
	if cfg.Cost(0, 0, 0, time.Hour) <= 0 {
		t.Error("a job requesting no CPUs cost nothing")
	}
}

// Shares are an entitlement: two accounts at 2 and 1 should tolerate usage in
// that ratio before either is penalised relative to the other.
func TestSharesSetTheEntitlement(t *testing.T) {
	cfg := Default().WithDefaults()
	cfg.Shares = map[string]float64{"big": 2, "small": 1}

	shares := cfg.SharesFor([]string{"big", "small"}, []Usage{
		{User: "big", ResourceSeconds: 200},
		{User: "small", ResourceSeconds: 100},
	})
	m := map[string]Share{}
	for _, s := range shares {
		m[s.User] = s
	}
	if !close2(m["big"].NormShares, 2.0/3.0) {
		t.Errorf("big norm shares = %v, want 2/3", m["big"].NormShares)
	}
	// Using exactly in proportion leaves both at the same factor.
	if !close2(m["big"].Factor, m["small"].Factor) {
		t.Errorf("proportional usage gave different factors: %v vs %v",
			m["big"].Factor, m["small"].Factor)
	}
	if !close2(m["big"].Factor, 0.5) {
		t.Errorf("factor at exactly the entitlement = %v, want 0.5", m["big"].Factor)
	}
}

// A brand new account has used nothing, so it must get the top factor rather
// than the zero a missing map entry would give.
func TestUnknownAccountIsNotPenalised(t *testing.T) {
	cfg := Default().WithDefaults()
	sc := NewScorer(cfg, cfg.SharesFor([]string{"known"}, nil), time.Unix(100, 0))
	got := sc.Score(Job{ID: 1, User: "never-seen", CPUs: 1})
	if got.Fair != 1 {
		t.Errorf("a new account scored fair-share %v, want 1", got.Fair)
	}
}

// An account with usage but no account row still consumed the cluster;
// ignoring it would inflate everyone else's share of what is left.
func TestUsageFromADeletedAccountStillCounts(t *testing.T) {
	cfg := Default().WithDefaults()
	shares := cfg.SharesFor([]string{"alice"}, []Usage{
		{User: "alice", ResourceSeconds: 100},
		{User: "ghost", ResourceSeconds: 100},
	})
	if len(shares) != 2 {
		t.Fatalf("got %d rows, want alice and ghost", len(shares))
	}
	for _, s := range shares {
		if s.User == "alice" && !close2(s.NormUsage, 0.5) {
			t.Errorf("alice's normalised usage = %v, want 0.5", s.NormUsage)
		}
	}
}

func TestSizeFactorAndItsDirection(t *testing.T) {
	cfg := Default().WithDefaults()
	cfg.Weights = Weights{Size: 1}
	now := time.Unix(100, 0)
	small := Job{ID: 1, User: "a", SubmitAt: now, CPUs: 1}
	big := Job{ID: 2, User: "a", SubmitAt: now, CPUs: 16}

	sc := NewScorer(cfg, nil, now)
	ordered, _ := sc.Order([]Job{small, big})
	if ordered[0].ID != 2 {
		t.Error("by default the size factor should favour the larger job")
	}

	cfg.FavorSmall = true
	sc = NewScorer(cfg, nil, now)
	ordered, _ = sc.Order([]Job{small, big})
	if ordered[0].ID != 1 {
		t.Error("favor_small did not reverse the size factor")
	}
}

// Ordering must be total and deterministic: a scheduler whose output depends
// on map iteration has bugs nobody can reproduce.
func TestOrderIsDeterministicOnTies(t *testing.T) {
	cfg := Default().WithDefaults()
	now := time.Unix(100, 0)
	jobs := []Job{
		{ID: 3, User: "a", SubmitAt: now, CPUs: 1},
		{ID: 1, User: "a", SubmitAt: now, CPUs: 1},
		{ID: 2, User: "a", SubmitAt: now, CPUs: 1},
	}
	sc := NewScorer(cfg, nil, now)
	var first []int64
	for i := 0; i < 20; i++ {
		ordered, _ := sc.Order(jobs)
		var ids []int64
		for _, j := range ordered {
			ids = append(ids, j.ID)
		}
		if first == nil {
			first = ids
			continue
		}
		for k := range ids {
			if ids[k] != first[k] {
				t.Fatalf("order varies between passes: %v then %v", first, ids)
			}
		}
	}
	// Equal priority falls back to submission order, then id.
	if first[0] != 1 || first[1] != 2 || first[2] != 3 {
		t.Errorf("tie-break order = %v, want ascending id", first)
	}
}

// A file that sets only `enabled: true` must produce a working policy, not a
// cluster where every job scores identically.
func TestWithDefaultsRepairsAPartialFile(t *testing.T) {
	c := Config{Enabled: true}.WithDefaults()
	if c.Weights.Sum() <= 0 {
		t.Error("weights left at zero, so every job would score the same")
	}
	if c.HalfLife <= 0 || c.MaxAge <= 0 || c.DefaultShares <= 0 ||
		c.Resource.GPU <= 0 || c.Resource.MemGB <= 0 || c.BackfillDepth <= 0 {
		t.Errorf("WithDefaults left something at zero: %+v", c)
	}
	// An explicit setting is not overwritten.
	c = Config{HalfLife: time.Hour, DefaultShares: 5,
		Weights: Weights{Age: 3}}.WithDefaults()
	if c.HalfLife != time.Hour || c.DefaultShares != 5 || c.Weights.Age != 3 {
		t.Errorf("WithDefaults overwrote explicit values: %+v", c)
	}
}

// Weights are relative: scaling them all must not change the order.
func TestWeightsAreRelative(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	run := func(w Weights) []int64 {
		cfg := Default().WithDefaults()
		cfg.Weights = w
		cfg.MaxAge = time.Hour
		shares := cfg.SharesFor([]string{"heavy", "idle"},
			[]Usage{{User: "heavy", ResourceSeconds: 10_000}})
		sc := NewScorer(cfg, shares, now)
		ordered, _ := sc.Order([]Job{
			{ID: 1, User: "heavy", SubmitAt: now.Add(-30 * time.Minute), CPUs: 1},
			{ID: 2, User: "idle", SubmitAt: now, CPUs: 4},
		})
		return []int64{ordered[0].ID, ordered[1].ID}
	}
	a := run(Weights{FairShare: 1, Age: 0.5, Size: 0.25})
	b := run(Weights{FairShare: 4, Age: 2, Size: 1})
	if a[0] != b[0] || a[1] != b[1] {
		t.Errorf("scaling every weight changed the order: %v vs %v", a, b)
	}
}

func TestDescribeNamesEveryFactor(t *testing.T) {
	s := Score{Priority: 0.5, Fair: 0.25, Age: 0.75, Size: 0.1}
	got := s.Describe()
	for _, want := range []string{"0.5000", "fair-share 0.25", "age 0.75", "size 0.10"} {
		if !contains(got, want) {
			t.Errorf("Describe() = %q, missing %q", got, want)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
