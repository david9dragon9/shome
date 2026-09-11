package agent

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/davidwu/shome/internal/job"
	"github.com/davidwu/shome/internal/platform"
)

// oomBackend answers the two questions the exit path asks of a backend, and
// nothing else: the embedded interface is nil, so any other call panics
// rather than quietly returning a zero value.
type oomBackend struct {
	platform.Backend
	after platform.Aftermath
	have  bool
}

func (b oomBackend) Sample(context.Context, *platform.Handle) (platform.Usage, error) {
	return platform.Usage{}, context.Canceled // no sample: this is about the exit path
}

func (b oomBackend) Aftermath(context.Context, *platform.Sandbox) (platform.Aftermath, bool) {
	return b.after, b.have
}

// finish runs one supervisor pass over a job that has just exited with code,
// and reports what the agent decided.
func finish(t *testing.T, be platform.Backend, limit int64, code int) (job.State, string, int64) {
	t.Helper()
	a := &Agent{Backend: be, Log: slog.New(slog.DiscardHandler),
		nowFn: time.Now, run: map[int64]*running{}, finished: map[int64]*finishedJob{}}
	ex := make(chan exit, 1)
	ex <- exit{code: code}
	a.run[1] = &running{
		id: 1, spec: job.Spec{Limits: job.Limits{MemBytes: limit}},
		sandbox: &platform.Sandbox{JobID: 1, ScratchDir: t.TempDir()},
		handle:  &platform.Handle{JobID: 1}, started: time.Now(), exited: ex,
	}
	a.checkAll(t.Context())
	if len(a.done) != 1 {
		t.Fatalf("the job did not finish: %+v", a.done)
	}
	d := a.done[0]
	return d.State, d.Reason, d.PeakMem
}

// A job that exits zero is still out of memory if the kernel killed
// something in it.
//
// This is the whole point of asking after the fact. A script that
// backgrounds its work and calls a bare `wait` reports zero whatever
// happened to the child, so the job used to be recorded as COMPLETED with a
// peak memory figure from whichever poll happened to land -- 85 MiB for a
// job the kernel killed at 264.
func TestAKernelOOMOutranksASuccessfulExitStatus(t *testing.T) {
	be := oomBackend{after: platform.Aftermath{OOMKills: 1, PeakBytes: 264 << 20}, have: true}
	st, reason, peak := finish(t, be, 200<<20, 0)
	if st != job.OOM {
		t.Errorf("state = %s, want %s -- the exit status hid the kill", st, job.OOM)
	}
	if !strings.Contains(reason, "264") || !strings.Contains(reason, "200") {
		t.Errorf("reason = %q, want the peak and the limit in it", reason)
	}
	if peak != 264<<20 {
		t.Errorf("peak = %d MiB, want the kernel's figure of 264", peak>>20)
	}
}

// And a job the kernel left alone is judged by its exit status, as before.
func TestWithoutAKernelOOMTheExitStatusDecides(t *testing.T) {
	quiet := oomBackend{after: platform.Aftermath{OOMKills: 0, PeakBytes: 6 << 20}, have: true}
	if st, _, peak := finish(t, quiet, 200<<20, 0); st != job.Completed || peak != 6<<20 {
		t.Errorf("state = %s, peak = %d MiB; want COMPLETED and the kernel's 6", st, peak>>20)
	}
	if st, reason, _ := finish(t, quiet, 200<<20, 3); st != job.Failed || !strings.Contains(reason, "exit code 3") {
		t.Errorf("state = %s, reason = %q; want FAILED naming the exit code", st, reason)
	}
	// A backend with no kernel accounting to read falls back to what it had
	// before: SIGKILL with a memory limit in force reads as OOM.
	silent := oomBackend{}
	if st, _, _ := finish(t, silent, 200<<20, exitKilled); st != job.OOM {
		t.Errorf("state = %s, want %s for a SIGKILL under a memory limit", st, job.OOM)
	}
	if st, _, _ := finish(t, silent, 0, exitKilled); st != job.Failed {
		t.Errorf("state = %s, want FAILED for a SIGKILL with no memory limit", st)
	}
}
