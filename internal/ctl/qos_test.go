package ctl

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/davidwu/shome/internal/job"
	"github.com/davidwu/shome/internal/qos"
	"github.com/davidwu/shome/internal/store"
)

func qosFixture(t *testing.T) (*Controller, *store.Store) {
	t.Helper()
	c, st := dashFixture(t)
	if _, err := st.CreateUser(context.Background(), "alice", store.RoleUser, 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	return c, st
}

func spec(cpus, gpus int, memMB int64, wall time.Duration) job.Spec {
	return job.Spec{Name: "j", User: "alice", Script: "true",
		Limits: job.Limits{CPUs: cpus, GPUs: gpus, MemBytes: memMB << 20, Walltime: wall}}
}

func TestCheckSubmitEnforcesPerJobCeilings(t *testing.T) {
	c, _ := qosFixture(t)
	ctx := context.Background()
	if err := c.SetUserQoS(ctx, "alice", qos.Limits{
		MaxCPUsPerJob: iptr(2), MaxGPUsPerJob: iptr(1),
		MaxMemMBPerJob: i64ptr(512), MaxWalltime: sptr("1h"),
	}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		s    job.Spec
		want string
	}{
		{"cpus", spec(8, 0, 0, 0), "max-cpus-per-job"},
		{"gpus", spec(1, 4, 0, 0), "max-gpus-per-job"},
		{"memory", spec(1, 0, 4096, 0), "max-mem-per-job"},
		{"walltime", spec(1, 0, 0, 8*time.Hour), "max-walltime"},
	} {
		err := c.CheckSubmit(ctx, "alice", tc.s, 1)
		if err == nil {
			t.Errorf("%s: over-limit request was accepted", tc.name)
			continue
		}
		// The message must name the limit, so somebody refused can find it.
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %q does not name %s", tc.name, err, tc.want)
		}
	}
	// And a request inside every ceiling passes.
	if err := c.CheckSubmit(ctx, "alice", spec(2, 1, 512, time.Hour), 1); err != nil {
		t.Errorf("a request at the limit was refused: %v", err)
	}
}

// An aggregate request is the same job asking for the same hardware by a
// different flag; letting it through would be a hole, not a distinction.
func TestCheckSubmitCoversAggregateRequests(t *testing.T) {
	c, _ := qosFixture(t)
	ctx := context.Background()
	c.SetUserQoS(ctx, "alice", qos.Limits{MaxGPUsPerJob: iptr(1), MaxCPUsPerJob: iptr(4)})

	s := job.Spec{Name: "j", User: "alice", Script: "true", TotalGPUs: 8}
	if err := c.CheckSubmit(ctx, "alice", s, 1); err == nil {
		t.Error("--total-gpus bypassed the per-job accelerator limit")
	}
	s = job.Spec{Name: "j", User: "alice", Script: "true", TotalCPUs: 64}
	if err := c.CheckSubmit(ctx, "alice", s, 1); err == nil {
		t.Error("--total-cpus bypassed the per-job CPU limit")
	}
}

// An array must not be able to step over the queue limit in one submission.
func TestCheckSubmitCountsArrayTasks(t *testing.T) {
	c, st := qosFixture(t)
	ctx := context.Background()
	c.SetUserQoS(ctx, "alice", qos.Limits{MaxSubmittedJobs: iptr(5)})

	if err := c.CheckSubmit(ctx, "alice", spec(1, 0, 0, 0), 10); err == nil {
		t.Fatal("a 10-task array was accepted against a limit of 5")
	}
	if err := c.CheckSubmit(ctx, "alice", spec(1, 0, 0, 0), 5); err != nil {
		t.Errorf("a 5-task array should fit exactly: %v", err)
	}
	// Fill the queue, then the same array must be refused.
	for i := 0; i < 5; i++ {
		if _, err := st.Submit(ctx, spec(1, 0, 0, 0), time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.CheckSubmit(ctx, "alice", spec(1, 0, 0, 0), 1); err == nil {
		t.Error("a submission over the queue limit was accepted")
	}
}

// Zero means none allowed, and must be enforced as such rather than read as
// "no limit".
func TestZeroLimitForbidsEntirely(t *testing.T) {
	c, _ := qosFixture(t)
	ctx := context.Background()
	c.SetUserQoS(ctx, "alice", qos.Limits{MaxGPUsPerJob: iptr(0)})
	if err := c.CheckSubmit(ctx, "alice", spec(1, 1, 0, 0), 1); err == nil {
		t.Fatal("a GPU job was accepted by an account limited to zero accelerators")
	}
	if err := c.CheckSubmit(ctx, "alice", spec(1, 0, 0, 0), 1); err != nil {
		t.Errorf("a CPU-only job was refused: %v", err)
	}
}

func TestAdmitForQoSHoldsBackOverLimitJobs(t *testing.T) {
	c, st := qosFixture(t)
	ctx := context.Background()
	c.SetUserQoS(ctx, "alice", qos.Limits{MaxRunningJobs: iptr(2)})
	st.UpsertNode(ctx, "n", "", caps(64, 65536, 4), time.Now())

	var pending []*job.Job
	for i := 0; i < 5; i++ {
		j, err := st.Submit(ctx, spec(1, 0, 0, 0), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		pending = append(pending, j)
	}
	admitted := c.admitForQoS(ctx, pending)
	if len(admitted) != 2 {
		t.Fatalf("admitted %d jobs, want 2", len(admitted))
	}
	// The rest must say why, not sit there silently.
	held, _ := st.Get(ctx, pending[4].ID)
	if !strings.Contains(held.Reason, "max-running-jobs") {
		t.Errorf("held job's reason is %q", held.Reason)
	}
}

// Admission must account for jobs admitted earlier in the same pass. Without
// that, an account one short of its limit has every queued job let through at
// once, because each was checked against a usage figure that excluded the
// others.
func TestAdmitForQoSCountsWithinOnePass(t *testing.T) {
	c, st := qosFixture(t)
	ctx := context.Background()
	c.SetUserQoS(ctx, "alice", qos.Limits{MaxRunningCPUs: iptr(4)})
	st.UpsertNode(ctx, "n", "", caps(64, 65536, 4), time.Now())

	var pending []*job.Job
	for i := 0; i < 4; i++ {
		j, _ := st.Submit(ctx, spec(2, 0, 0, 0), time.Now()) // 2 CPUs each
		pending = append(pending, j)
	}
	if got := len(c.admitForQoS(ctx, pending)); got != 2 {
		t.Fatalf("admitted %d jobs (%d CPUs), want 2 to fit a 4-CPU limit", got, got*2)
	}
}

func TestQoSDefaultsFileRoundTrip(t *testing.T) {
	c, _ := qosFixture(t)
	if err := c.SetQoSDefaults(qos.Limits{MaxRunningJobs: iptr(7)}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(QoSFile(c.root))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "max_running_jobs: 7") {
		t.Errorf("file does not contain the setting:\n%s", b)
	}
	// It is meant to be human-editable, so it should explain itself.
	if !strings.Contains(string(b), "#") {
		t.Error("the file has no explanatory comment")
	}
	if got := c.QoSDefaults(); got.MaxRunningJobs == nil || *got.MaxRunningJobs != 7 {
		t.Errorf("reload produced %v", got.MaxRunningJobs)
	}
}

// An unparseable file must not read as "no limits". Failing open would turn a
// typo into an unbounded cluster.
func TestCorruptQoSFileKeepsLimits(t *testing.T) {
	c, _ := qosFixture(t)
	c.SetQoSDefaults(qos.Limits{MaxRunningJobs: iptr(3)})
	if got := c.QoSDefaults(); *got.MaxRunningJobs != 3 {
		t.Fatal("setup")
	}
	if err := os.WriteFile(QoSFile(c.root), []byte("max_running_jobs: [this is not a number"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := c.QoSDefaults()
	if got.MaxRunningJobs == nil || *got.MaxRunningJobs != 3 {
		t.Errorf("a corrupt file dropped the limits: %v", got.MaxRunningJobs)
	}
}

// The process limit is decided by the controller and carried on the job,
// because only the node running it can count processes. Taking it from what
// the submitter sent would let anyone raise their own.
func TestApplyQoSStampsProcessLimitAndWalltime(t *testing.T) {
	c, _ := qosFixture(t)
	ctx := context.Background()
	c.SetUserQoS(ctx, "alice", qos.Limits{MaxProcsPerJob: iptr(16), MaxWalltime: sptr("30m")})

	s := spec(1, 0, 0, 0)
	s.Limits.MaxProcs = 999999 // what a hostile submitter would send
	c.ApplyQoS(ctx, "alice", &s)
	if s.Limits.MaxProcs != 16 {
		t.Errorf("MaxProcs = %d, want the account's 16", s.Limits.MaxProcs)
	}
	if s.Limits.Walltime != 30*time.Minute {
		t.Errorf("Walltime = %v, want the account ceiling inherited", s.Limits.Walltime)
	}
	// An explicit, shorter walltime is kept.
	s2 := spec(1, 0, 0, 5*time.Minute)
	c.ApplyQoS(ctx, "alice", &s2)
	if s2.Limits.Walltime != 5*time.Minute {
		t.Errorf("an explicit --time was overwritten: %v", s2.Limits.Walltime)
	}
}

func iptr(v int) *int       { return &v }
func i64ptr(v int64) *int64 { return &v }
func sptr(v string) *string { return &v }

// The cluster ceiling and the per-account default answer different questions,
// and the cluster one is the only thing that actually bounds a cluster:
// per-account limits multiply, so ten accounts allowed four accelerators each
// is forty, whatever the hardware.
func TestClusterCeilingBindsAcrossUsers(t *testing.T) {
	c, st := qosFixture(t)
	ctx := context.Background()
	if _, err := st.CreateUser(ctx, "bob", store.RoleUser, 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	st.UpsertNode(ctx, "n", "", caps(64, 65536, 8), time.Now())

	// Each account may run plenty; the cluster allows three.
	if err := c.SetQoSConfig(qos.Config{
		Cluster: qos.Limits{MaxRunningJobs: iptr(3)},
		PerUser: qos.Limits{MaxRunningJobs: iptr(10)},
	}); err != nil {
		t.Fatal(err)
	}

	var pending []*job.Job
	for _, u := range []string{"alice", "alice", "alice", "bob", "bob"} {
		s := spec(1, 0, 0, 0)
		s.User = u
		j, err := st.Submit(ctx, s, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		pending = append(pending, j)
	}
	admitted := c.admitForQoS(ctx, pending)
	if len(admitted) != 3 {
		t.Fatalf("admitted %d jobs, want the cluster's 3 -- per-account limits "+
			"alone would have allowed all 5", len(admitted))
	}
	held, _ := st.Get(ctx, pending[4].ID)
	if !strings.Contains(held.Reason, "cluster") {
		t.Errorf("held job blames the wrong thing: %q", held.Reason)
	}
}

// A per-account override must not be able to exceed the cluster ceiling:
// raising one person's allowance is not the same decision as raising what the
// cluster will hand out.
func TestPerAccountOverrideCannotExceedClusterCeiling(t *testing.T) {
	c, _ := qosFixture(t)
	ctx := context.Background()
	c.SetQoSConfig(qos.Config{
		Cluster: qos.Limits{MaxGPUsPerJob: iptr(1), MaxMemMBPerJob: i64ptr(1024)},
		PerUser: qos.Defaults(),
	})
	// alice is generously overridden, well past the cluster ceiling.
	c.SetUserQoS(ctx, "alice", qos.Limits{MaxGPUsPerJob: iptr(8), MaxMemMBPerJob: i64ptr(65536)})

	if err := c.CheckSubmit(ctx, "alice", spec(1, 4, 0, 0), 1); err == nil {
		t.Error("an account override exceeded the cluster accelerator ceiling")
	} else if !strings.Contains(err.Error(), "cluster") {
		t.Errorf("the refusal does not say it is a cluster limit: %v", err)
	}
	if err := c.CheckSubmit(ctx, "alice", spec(1, 0, 4096, 0), 1); err == nil {
		t.Error("an account override exceeded the cluster memory ceiling")
	}
	// Within the cluster ceiling, her override still applies.
	if err := c.CheckSubmit(ctx, "alice", spec(1, 1, 1024, 0), 1); err != nil {
		t.Errorf("a request inside both limits was refused: %v", err)
	}
}

// The two layers are separate settings: changing one must not disturb the
// other, or an admin tightening the cluster would silently reset everybody.
func TestLayersAreIndependent(t *testing.T) {
	c, _ := qosFixture(t)
	if err := c.SetQoSDefaults(qos.Limits{MaxRunningJobs: iptr(5)}); err != nil {
		t.Fatal(err)
	}
	if err := c.SetQoSCluster(qos.Limits{MaxRunningGPUs: iptr(2)}); err != nil {
		t.Fatal(err)
	}
	cfg := c.QoSConfig()
	if cfg.PerUser.MaxRunningJobs == nil || *cfg.PerUser.MaxRunningJobs != 5 {
		t.Errorf("setting the cluster layer disturbed the per-account one: %v", cfg.PerUser.MaxRunningJobs)
	}
	if cfg.Cluster.MaxRunningGPUs == nil || *cfg.Cluster.MaxRunningGPUs != 2 {
		t.Errorf("cluster layer = %v", cfg.Cluster.MaxRunningGPUs)
	}
}

// Cluster totals default to unlimited, because shome cannot know how big the
// cluster is and a made-up ceiling would refuse work a machine could run.
func TestClusterTotalsDefaultToUnlimited(t *testing.T) {
	e := qos.DefaultConfig().ClusterEffective()
	if e.MaxRunningJobs != qos.Unlimited || e.MaxRunningGPUs != qos.Unlimited {
		t.Errorf("cluster defaults are not unlimited: jobs=%d gpus=%d",
			e.MaxRunningJobs, e.MaxRunningGPUs)
	}
	// While the per-account layer is still bounded.
	if p := qos.DefaultConfig().ResolveFor(qos.Limits{}); p.MaxRunningJobs <= 0 {
		t.Error("per-account defaults are unbounded")
	}
}

// A file written before the split has its limits at the top level and means
// them per-account. Reading it as the new shape would drop every one.
func TestLegacyFlatFileIsReadAsPerAccount(t *testing.T) {
	c, _ := qosFixture(t)
	if err := os.WriteFile(QoSFile(c.root),
		[]byte("max_running_jobs: 4\nmax_disk_mb: 2048\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := c.QoSConfig()
	if cfg.PerUser.MaxRunningJobs == nil || *cfg.PerUser.MaxRunningJobs != 4 {
		t.Fatalf("legacy limits were dropped: %+v", cfg.PerUser)
	}
	if cfg.Cluster.MaxRunningJobs != nil {
		t.Error("legacy limits were read as cluster totals rather than per-account")
	}
}

// An account over its storage limit does not get new work started.
//
// A job's writes cannot be capped: there are no OS accounts and so no
// filesystem quota, and the account's directory is writable by design. So
// an account that had already filled its allowance could keep filling other
// people's disks through jobs, while `shome storage put` refused it a single
// byte. Admission is the only lever there is.
func TestAdmitForQoSHoldsBackAnAccountOverItsStorageLimit(t *testing.T) {
	c, st := qosFixture(t)
	ctx := context.Background()
	c.SetUserQoS(ctx, "alice", qos.Limits{MaxDiskMB: i64ptr(10)})
	st.UpsertNode(ctx, "n", "", caps(64, 65536, 4), time.Now())

	j, err := st.Submit(ctx, spec(1, 0, 0, 0), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// Within the limit: it runs.
	if err := st.ReplaceNodeFiles(ctx, "n", "alice", nil,
		store.NodeUsage{Node: "n", User: "alice", Bytes: 4 << 20, Files: 1}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := len(c.admitForQoS(ctx, []*job.Job{j})); got != 1 {
		t.Fatalf("admitted %d jobs, want 1 -- the account is within its limit", got)
	}

	// Over it: held, with a reason that says what to do.
	if err := st.ReplaceNodeFiles(ctx, "n", "alice", nil,
		store.NodeUsage{Node: "n", User: "alice", Bytes: 50 << 20, Files: 2}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := len(c.admitForQoS(ctx, []*job.Job{j})); got != 0 {
		t.Fatalf("admitted %d jobs, want 0 -- the account is over its limit", got)
	}
	held, _ := st.Get(ctx, j.ID)
	for _, want := range []string{"storage limit", "50 MiB", "10 MiB", "shome fs rm"} {
		if !strings.Contains(held.Reason, want) {
			t.Errorf("reason = %q, missing %q", held.Reason, want)
		}
	}

	// Freeing space makes it runnable again, without resubmitting.
	if err := st.ReplaceNodeFiles(ctx, "n", "alice", nil,
		store.NodeUsage{Node: "n", User: "alice", Bytes: 1 << 20, Files: 1}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := len(c.admitForQoS(ctx, []*job.Job{j})); got != 1 {
		t.Errorf("admitted %d jobs after freeing space, want 1", got)
	}
}

// An account whose storage limit is explicitly unlimited is never held back
// by one, however much it is using.
func TestAdmitForQoSIgnoresStorageWhenUnlimited(t *testing.T) {
	c, st := qosFixture(t)
	ctx := context.Background()
	c.SetUserQoS(ctx, "alice", qos.Limits{MaxDiskMB: i64ptr(-1)})
	st.UpsertNode(ctx, "n", "", caps(64, 65536, 4), time.Now())
	j, _ := st.Submit(ctx, spec(1, 0, 0, 0), time.Now())
	st.ReplaceNodeFiles(ctx, "n", "alice", nil,
		store.NodeUsage{Node: "n", User: "alice", Bytes: 500 << 30, Files: 9}, time.Now())
	if got := len(c.admitForQoS(ctx, []*job.Job{j})); got != 1 {
		t.Errorf("admitted %d jobs, want 1 -- there is no limit to be over", got)
	}
}

// Every running-usage limit is enforced, not just the two that are easy to
// reach. Each of these is a separate branch, and a limit with no test is one
// refactor away from being the limit that quietly stopped applying.
func TestEveryRunningLimitHoldsBackWork(t *testing.T) {
	for _, tc := range []struct {
		name   string
		limits qos.Limits
		spec   job.Spec
		want   int // jobs admitted out of three
		blames string
	}{
		{"jobs", qos.Limits{MaxRunningJobs: iptr(1)}, spec(1, 0, 0, 0), 1, "max-running-jobs"},
		{"cpus", qos.Limits{MaxRunningCPUs: iptr(3)}, spec(2, 0, 0, 0), 1, "max-running-cpus"},
		{"gpus", qos.Limits{MaxRunningGPUs: iptr(2)}, spec(1, 1, 0, 0), 2, "max-running-gpus"},
		{"memory", qos.Limits{MaxRunningMemMB: i64ptr(1024)}, spec(1, 0, 512, 0), 2, "max-running-mem"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, st := qosFixture(t)
			ctx := context.Background()
			if err := c.SetUserQoS(ctx, "alice", tc.limits); err != nil {
				t.Fatal(err)
			}
			st.UpsertNode(ctx, "n", "", caps(64, 65536, 8), time.Now())

			var pending []*job.Job
			for i := 0; i < 3; i++ {
				j, err := st.Submit(ctx, tc.spec, time.Now())
				if err != nil {
					t.Fatal(err)
				}
				pending = append(pending, j)
			}
			if got := len(c.admitForQoS(ctx, pending)); got != tc.want {
				t.Fatalf("admitted %d jobs, want %d", got, tc.want)
			}
			// The job held back has to say which limit held it, or the only way
			// to find out is to ask an admin.
			held, err := st.Get(ctx, pending[2].ID)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(held.Reason, tc.blames) {
				t.Errorf("held job's reason is %q, expected it to name %s", held.Reason, tc.blames)
			}
		})
	}
}

// Zero means none allowed at the admission layer too, not only at submission.
// A running limit of zero is how an admin says "this account may queue work
// but nothing of it may start", which is what a soft quarantine is.
func TestZeroRunningLimitStartsNothing(t *testing.T) {
	c, st := qosFixture(t)
	ctx := context.Background()
	c.SetUserQoS(ctx, "alice", qos.Limits{MaxRunningJobs: iptr(0)})
	st.UpsertNode(ctx, "n", "", caps(64, 65536, 4), time.Now())
	j, err := st.Submit(ctx, spec(1, 0, 0, 0), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got := len(c.admitForQoS(ctx, []*job.Job{j})); got != 0 {
		t.Fatalf("admitted %d jobs against a limit of zero", got)
	}
}

// An explicitly unlimited running limit admits work no matter how much is
// already going. Checked alongside the zero case because the two are one
// character apart in the file and mean opposite things.
func TestUnlimitedRunningLimitAdmitsEverything(t *testing.T) {
	c, st := qosFixture(t)
	ctx := context.Background()
	c.SetQoSConfig(qos.Config{
		Cluster: qos.Limits{},
		PerUser: qos.Limits{MaxRunningJobs: iptr(-1), MaxRunningCPUs: iptr(-1),
			MaxRunningGPUs: iptr(-1), MaxRunningMemMB: i64ptr(-1), MaxDiskMB: i64ptr(-1)},
	})
	st.UpsertNode(ctx, "n", "", caps(64, 65536, 8), time.Now())
	var pending []*job.Job
	for i := 0; i < 12; i++ {
		j, _ := st.Submit(ctx, spec(4, 1, 4096, 0), time.Now())
		pending = append(pending, j)
	}
	if got := len(c.admitForQoS(ctx, pending)); got != 12 {
		t.Errorf("admitted %d of 12 jobs with every limit unlimited", got)
	}
}

// The cluster's queue depth is a separate ceiling from an account's, and it is
// the one that bounds the database: without it a single account with a
// generous per-account limit can still be the whole queue.
func TestClusterQueueDepthRefusesFurtherSubmissions(t *testing.T) {
	c, st := qosFixture(t)
	ctx := context.Background()
	if _, err := st.CreateUser(ctx, "bob", store.RoleUser, 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := c.SetQoSConfig(qos.Config{
		Cluster: qos.Limits{MaxSubmittedJobs: iptr(3)},
		PerUser: qos.Limits{MaxSubmittedJobs: iptr(100)},
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := st.Submit(ctx, spec(1, 0, 0, 0), time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	// bob is well inside his own limit and still refused: the queue is full.
	s := spec(1, 0, 0, 0)
	s.User = "bob"
	err := c.CheckSubmit(ctx, "bob", s, 1)
	if err == nil {
		t.Fatal("a submission past the cluster queue limit was accepted")
	}
	for _, want := range []string{"cluster", "not yours"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not say %q -- somebody refused by a "+
				"cluster-wide limit should not go looking at their own", err, want)
		}
	}
	// And an array cannot step over it in one go either.
	if err := c.CheckSubmit(ctx, "bob", s, 50); err == nil {
		t.Error("a 50-task array was accepted against a full cluster queue")
	}
}

// A per-account override cannot exceed the cluster's walltime or CPU ceiling
// either. The ceiling covers every per-job dimension or it covers none of
// them usefully: a job is bounded by its weakest limit.
func TestClusterCeilingCoversEveryPerJobDimension(t *testing.T) {
	c, _ := qosFixture(t)
	ctx := context.Background()
	c.SetQoSConfig(qos.Config{
		Cluster: qos.Limits{MaxCPUsPerJob: iptr(2), MaxGPUsPerJob: iptr(1),
			MaxMemMBPerJob: i64ptr(1024), MaxWalltime: sptr("1h")},
		PerUser: qos.Defaults(),
	})
	c.SetUserQoS(ctx, "alice", qos.Limits{MaxCPUsPerJob: iptr(64),
		MaxGPUsPerJob: iptr(8), MaxMemMBPerJob: i64ptr(65536), MaxWalltime: sptr("7d")})

	for _, tc := range []struct {
		name string
		s    job.Spec
	}{
		{"cpus", spec(8, 0, 0, 0)},
		{"gpus", spec(1, 4, 0, 0)},
		{"memory", spec(1, 0, 8192, 0)},
		{"walltime", spec(1, 0, 0, 24*time.Hour)},
	} {
		err := c.CheckSubmit(ctx, "alice", tc.s, 1)
		if err == nil {
			t.Errorf("%s: an account override exceeded the cluster ceiling", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), "cluster") {
			t.Errorf("%s: the refusal does not say it is a cluster limit: %v", tc.name, err)
		}
	}
	// Inside every ceiling, the job is accepted.
	if err := c.CheckSubmit(ctx, "alice", spec(2, 1, 1024, time.Hour), 1); err != nil {
		t.Errorf("a request inside every ceiling was refused: %v", err)
	}
}

// The process limit is the one limit a submitter could otherwise raise for
// themselves, because it has to travel on the job for the node to apply it.
// An account with no limit set must still not be able to send its own.
func TestASubmitterCannotSendItsOwnProcessLimit(t *testing.T) {
	c, _ := qosFixture(t)
	ctx := context.Background()
	// No override at all: the account gets the cluster default, whatever it is.
	s := spec(1, 0, 0, 0)
	s.Limits.MaxProcs = 1 << 20
	c.ApplyQoS(ctx, "alice", &s)
	want := qos.DefaultConfig().ResolveFor(qos.Limits{}).MaxProcsPerJob
	if want < 0 {
		t.Skip("the default process limit is unlimited; nothing to overwrite")
	}
	if s.Limits.MaxProcs != want {
		t.Errorf("MaxProcs = %d, want the account's %d -- a submitted value must "+
			"never survive", s.Limits.MaxProcs, want)
	}
}
