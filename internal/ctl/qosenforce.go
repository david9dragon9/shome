package ctl

import (
	"context"
	"fmt"

	"github.com/davidwu/shome/internal/job"
	"github.com/davidwu/shome/internal/qos"
	"github.com/davidwu/shome/internal/sched"
)

// Enforcement.
//
// Split across two moments because the limits divide naturally in two:
//
//   - What one job asks for is knowable at submission, so it is refused there,
//     with the number it exceeded. Accepting a job that can never be scheduled
//     and leaving it PENDING forever would be a worse answer than "no".
//   - What an account is using depends on what is running now, so it is
//     checked when a job is about to be placed. A job over that line stays
//     queued with a reason, and runs when the account's other work finishes.
//
// Nothing is silently clamped. A request reshaped to fit a limit is a job that
// does something other than what was asked, which is harder to notice than a
// refusal and worse when it happens.

// ApplyQoS stamps limits the runtime must enforce onto a job's spec.
//
// Process count travels with the job because only the node running it can
// count its processes; the controller decides the number, the agent applies
// it. Taking it from the spec the submitter sent would let anyone raise their
// own limit.
func (c *Controller) ApplyQoS(ctx context.Context, user string, spec *job.Spec) {
	e, err := c.EffectiveQoS(ctx, user)
	if err != nil {
		return
	}
	if e.MaxProcsPerJob >= 0 {
		spec.Limits.MaxProcs = e.MaxProcsPerJob
	}
	// An unspecified walltime inherits the account ceiling, so a job cannot
	// run forever merely by omitting --time.
	if spec.Limits.Walltime == 0 && e.MaxWalltime > 0 {
		spec.Limits.Walltime = e.MaxWalltime
	}
}

// CheckSubmit rejects a job whose own request exceeds the account's ceilings.
func (c *Controller) CheckSubmit(ctx context.Context, user string, spec job.Spec, tasks int) error {
	e, err := c.EffectiveQoS(ctx, user)
	if err != nil {
		return err
	}

	if e.MaxCPUsPerJob >= 0 && spec.Limits.CPUs > e.MaxCPUsPerJob {
		return limitErr("cpus-per-task", spec.Limits.CPUs, e.MaxCPUsPerJob, "max-cpus-per-job")
	}
	if e.MaxGPUsPerJob >= 0 && spec.Limits.GPUs > e.MaxGPUsPerJob {
		return limitErr("gres=gpu", spec.Limits.GPUs, e.MaxGPUsPerJob, "max-gpus-per-job")
	}
	if e.MaxMemMBPerJob >= 0 {
		if mb := spec.Limits.MemBytes >> 20; mb > e.MaxMemMBPerJob {
			return limitErr64("mem", mb, e.MaxMemMBPerJob, "max-mem-per-job", "MiB")
		}
	}
	if e.MaxWalltime > 0 && spec.Limits.Walltime > e.MaxWalltime {
		return fmt.Errorf("--time %s exceeds your limit of %s (max-walltime)",
			job.FormatDuration(spec.Limits.Walltime), e.MaxWalltime)
	}

	// Aggregate requests are checked against the same per-job ceilings: a
	// --total-gpus of 8 is one job using eight accelerators, and letting it
	// through because it took a different flag would be a hole rather than a
	// distinction.
	if e.MaxGPUsPerJob >= 0 && spec.TotalGPUs > e.MaxGPUsPerJob {
		return limitErr("total-gpus", spec.TotalGPUs, e.MaxGPUsPerJob, "max-gpus-per-job")
	}
	if e.MaxCPUsPerJob >= 0 && spec.TotalCPUs > e.MaxCPUsPerJob {
		return limitErr("total-cpus", spec.TotalCPUs, e.MaxCPUsPerJob, "max-cpus-per-job")
	}
	if e.MaxMemMBPerJob >= 0 && spec.TotalMemBytes>>20 > e.MaxMemMBPerJob {
		return limitErr64("total-mem", spec.TotalMemBytes>>20, e.MaxMemMBPerJob, "max-mem-per-job", "MiB")
	}

	// The cluster's own ceilings. An account's limits may be raised without
	// anybody meaning to raise what the cluster as a whole will hand out, so
	// no per-account override can exceed these.
	cl := c.QoSConfig().ClusterEffective()
	if cl.MaxCPUsPerJob >= 0 && spec.Limits.CPUs > cl.MaxCPUsPerJob {
		return fmt.Errorf("--cpus-per-task %d exceeds the cluster ceiling of %d "+
			"(cluster max-cpus-per-job)", spec.Limits.CPUs, cl.MaxCPUsPerJob)
	}
	if cl.MaxGPUsPerJob >= 0 && spec.Limits.GPUs > cl.MaxGPUsPerJob {
		return fmt.Errorf("--gres=gpu %d exceeds the cluster ceiling of %d "+
			"(cluster max-gpus-per-job)", spec.Limits.GPUs, cl.MaxGPUsPerJob)
	}
	if cl.MaxMemMBPerJob >= 0 && spec.Limits.MemBytes>>20 > cl.MaxMemMBPerJob {
		return fmt.Errorf("--mem %d MiB exceeds the cluster ceiling of %d MiB "+
			"(cluster max-mem-per-job)", spec.Limits.MemBytes>>20, cl.MaxMemMBPerJob)
	}
	if cl.MaxWalltime > 0 && spec.Limits.Walltime > cl.MaxWalltime {
		return fmt.Errorf("--time %s exceeds the cluster ceiling of %s (cluster max-walltime)",
			job.FormatDuration(spec.Limits.Walltime), cl.MaxWalltime)
	}
	if cl.MaxSubmittedJobs >= 0 {
		n, err := c.store.CountActive(ctx)
		if err != nil {
			return err
		}
		t := tasks
		if t < 1 {
			t = 1
		}
		if n+t > cl.MaxSubmittedJobs {
			return fmt.Errorf(
				"the cluster queue is full: %d of %d slots in use (cluster max-submitted-jobs).\n"+
					"This is a cluster-wide limit, not yours; try again shortly", n, cl.MaxSubmittedJobs)
		}
	}

	// Queue depth. Counted including the tasks this submission would add, so
	// an array cannot step over the line in one go.
	if e.MaxSubmittedJobs >= 0 {
		n, err := c.store.CountActiveForUser(ctx, user)
		if err != nil {
			return err
		}
		if tasks < 1 {
			tasks = 1
		}
		if n+tasks > e.MaxSubmittedJobs {
			return fmt.Errorf(
				"this would give you %d queued or running job(s), over your limit of %d "+
					"(max-submitted-jobs).\nYou have %d now; wait for some to finish, or cancel a few",
				n+tasks, e.MaxSubmittedJobs, n)
		}
	}
	return nil
}

// UsageOf is what an account currently has running.
type UsageOf struct {
	Jobs  int
	CPUs  int
	GPUs  int
	MemMB int64
}

// RunningUsage sums running allocations for one account, or for the whole
// cluster when user is empty.
func (c *Controller) RunningUsage(ctx context.Context, user string) (UsageOf, error) {
	jobs, err := c.store.List(ctx, user, true)
	if err != nil {
		return UsageOf{}, err
	}
	var u UsageOf
	for _, j := range jobs {
		if j.State != job.Running {
			continue
		}
		u.Jobs++
		u.CPUs += j.Spec.Limits.CPUs
		u.GPUs += j.Spec.Limits.GPUs
		u.MemMB += j.Spec.Limits.MemBytes >> 20
	}
	return u, nil
}

// admitForQoS filters a scheduling round down to jobs their owners may start
// right now, and says why for the ones held back.
//
// Accounting for jobs admitted earlier in the same pass matters: without it,
// an account at its limit minus one would have every queued job admitted in a
// single round, because each was checked against a usage figure that did not
// yet include the others.
func (c *Controller) admitForQoS(ctx context.Context, jobs []*job.Job) []*job.Job {
	if len(jobs) == 0 {
		return jobs
	}
	usage := map[string]UsageOf{}
	limits := map[string]qos.Effective{}
	// Why an account may not start work because of its files, cached for
	// this round. Empty string means it may.
	diskWhy := map[string]string{}
	out := make([]*job.Job, 0, len(jobs))

	// The cluster's own running totals, across everybody. Tracked alongside
	// the per-account figures because a round that admits within every
	// account's limit can still admit more than the cluster allows.
	cl := c.QoSConfig().ClusterEffective()
	clUsage, err := c.RunningUsage(ctx, "")
	if err != nil {
		c.log.Error("cluster usage", "err", err)
		clUsage = UsageOf{}
	}

	for _, j := range jobs {
		user := j.Spec.User
		if _, ok := usage[user]; !ok {
			u, err := c.RunningUsage(ctx, user)
			if err != nil {
				c.log.Error("qos usage", "user", user, "err", err)
				out = append(out, j) // never block work on a bookkeeping failure
				continue
			}
			e, err := c.EffectiveQoS(ctx, user)
			if err != nil {
				c.log.Error("qos limits", "user", user, "err", err)
				out = append(out, j)
				continue
			}
			usage[user], limits[user] = u, e
			diskWhy[user] = c.overDisk(ctx, user, e)
		}
		u, e := usage[user], limits[user]

		// An account over its storage limit does not get new work started.
		//
		// A job cannot be stopped from writing: there are no OS-level
		// accounts and therefore no filesystem quota, so what a job writes
		// into the account's directory is bounded by nothing. Admission is
		// the only lever there is, and without it an account already over
		// its limit could keep filling other people's disks indefinitely --
		// while `shome storage put` refused it a single byte.
		//
		// Held rather than failed: freeing space makes the job runnable, and
		// the reason says what to free.
		if why := diskWhy[user]; why != "" {
			c.store.SetReason(ctx, j.ID, why)
			continue
		}

		want := UsageOf{
			Jobs: 1, CPUs: j.Spec.Limits.CPUs, GPUs: j.Spec.Limits.GPUs,
			MemMB: j.Spec.Limits.MemBytes >> 20,
		}
		if why := overLimit(u, want, e, "your"); why != "" {
			c.store.SetReason(ctx, j.ID, why)
			continue
		}
		if why := overLimit(clUsage, want, cl, "the cluster's"); why != "" {
			c.store.SetReason(ctx, j.ID, "waiting for capacity: "+why)
			continue
		}
		add := func(a, b UsageOf) UsageOf {
			return UsageOf{Jobs: a.Jobs + b.Jobs, CPUs: a.CPUs + b.CPUs,
				GPUs: a.GPUs + b.GPUs, MemMB: a.MemMB + b.MemMB}
		}
		usage[user] = add(u, want)
		clUsage = add(clUsage, want)
		out = append(out, j)
	}
	return out
}

// overDisk reports why an account's files stop it starting new work.
//
// Measured from what the machines reported rather than from a running tally,
// for the reason the whole file index is: a limit enforced against a
// drifting number eventually locks somebody out of space that is free.
func (c *Controller) overDisk(ctx context.Context, user string, e qos.Effective) string {
	if e.MaxDiskMB < 0 {
		return ""
	}
	bytes, _, err := c.store.UserDiskTotal(ctx, user)
	if err != nil {
		c.log.Error("disk usage for admission", "user", user, "err", err)
		return "" // never block work on a bookkeeping failure
	}
	limit := e.MaxDiskMB << 20
	if bytes <= limit {
		return ""
	}
	return fmt.Sprintf("your files are over your storage limit "+
		"(%d MiB used, %d MiB allowed); free some with 'shome fs rm' (max-disk)",
		bytes>>20, e.MaxDiskMB)
}

// overLimit reports the first limit adding want to u would break.
//
// whose distinguishes the two callers, because "you are at your limit" and
// "the cluster is at its limit" call for different reactions from whoever
// reads it -- one is a queue to wait out, the other is a reason to ask an
// admin for more.
func overLimit(u, want UsageOf, e qos.Effective, whose string) string {
	if e.MaxRunningJobs >= 0 && u.Jobs+want.Jobs > e.MaxRunningJobs {
		return fmt.Sprintf("at %s limit of %d running job(s) (max-running-jobs)",
			whose, e.MaxRunningJobs)
	}
	if e.MaxRunningCPUs >= 0 && u.CPUs+want.CPUs > e.MaxRunningCPUs {
		return fmt.Sprintf("would use %d of %s %d CPU limit (max-running-cpus)",
			u.CPUs+want.CPUs, whose, e.MaxRunningCPUs)
	}
	if e.MaxRunningGPUs >= 0 && u.GPUs+want.GPUs > e.MaxRunningGPUs {
		return fmt.Sprintf("would use %d of %s %d accelerator limit (max-running-gpus)",
			u.GPUs+want.GPUs, whose, e.MaxRunningGPUs)
	}
	if e.MaxRunningMemMB >= 0 && u.MemMB+want.MemMB > e.MaxRunningMemMB {
		return fmt.Sprintf("would use %d MiB of %s %d MiB memory limit (max-running-mem)",
			u.MemMB+want.MemMB, whose, e.MaxRunningMemMB)
	}
	return ""
}

func limitErr(what string, got, max int, flag string) error {
	return fmt.Errorf("--%s %d exceeds your limit of %d (%s)", what, got, max, flag)
}

func limitErr64(what string, got, max int64, flag, unit string) error {
	return fmt.Errorf("--%s %d %s exceeds your limit of %d %s (%s)", what, got, unit, max, unit, flag)
}

var _ = sched.NodeState{}
