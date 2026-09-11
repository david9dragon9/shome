//go:build darwin

package darwin

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/davidwu/shome/internal/job"
	"github.com/davidwu/shome/internal/platform"
)

func newTestBackend(t *testing.T) (*Backend, string) {
	t.Helper()
	root := t.TempDir()
	home := t.TempDir() // stand-in owner home
	if err := os.WriteFile(filepath.Join(home, "secret.txt"), []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	return New(root, home), home
}

func runScript(t *testing.T, b *Backend, id int64, script string, lim job.Limits) (*job.Spec, string, int) {
	t.Helper()
	ctx := context.Background()
	spec := job.Spec{Name: "t", User: "tester", Args: []string{"/bin/sh", "-c", script}, Limits: lim}
	sb, err := b.Prepare(ctx, spec, id)
	if err != nil {
		t.Fatal(err)
	}
	h, err := b.Launch(ctx, sb, spec)
	if err != nil {
		t.Fatal(err)
	}
	code, err := b.Wait(ctx, h)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := os.ReadFile(filepath.Join(sb.ScratchDir, "slurm-"+strconv.FormatInt(id, 10)+".out"))
	return &spec, string(out), code
}

func TestJobRunsAndCapturesOutput(t *testing.T) {
	b, _ := newTestBackend(t)
	_, out, code := runScript(t, b, 1, `echo hello-from-job; exit 0`, job.Limits{})
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if !strings.Contains(out, "hello-from-job") {
		t.Errorf("output missing; got %q", out)
	}
}

func TestExitCodePropagates(t *testing.T) {
	b, _ := newTestBackend(t)
	if _, _, code := runScript(t, b, 2, `exit 42`, job.Limits{}); code != 42 {
		t.Errorf("exit code = %d, want 42", code)
	}
}

func TestJobCannotReadOwnerHome(t *testing.T) {
	b, home := newTestBackend(t)
	_, out, code := runScript(t, b, 3, `cat `+filepath.Join(home, "secret.txt"), job.Limits{})
	if code == 0 || strings.Contains(out, "private") {
		t.Errorf("job read the owner's home: code=%d out=%q", code, out)
	}
}

func TestJobsCannotSeeEachOther(t *testing.T) {
	b, _ := newTestBackend(t)
	ctx := context.Background()

	victim := job.Spec{Args: []string{"/bin/sh", "-c", "sleep 0.1"}}
	vsb, err := b.Prepare(ctx, victim, 10)
	if err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(vsb.ScratchDir, "victim.txt")
	if err := os.WriteFile(secret, []byte("victim-data"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, out, code := runScript(t, b, 11, "cat "+secret, job.Limits{})
	if code == 0 || strings.Contains(out, "victim-data") {
		t.Errorf("job 11 read job 10's scratch: code=%d out=%q", code, out)
	}
}

// Regression test: enforcement must catch a FORKED child, which is
// exactly what `taskpolicy -m` fails to do.
func TestSampleAccountsForForkedChild(t *testing.T) {
	b, _ := newTestBackend(t)
	ctx := context.Background()
	// dd rather than python3: the system python on macOS is an Xcode stub
	// that needs the developer directory, which the sandbox does not expose.
	// dd allocates and touches a real buffer of the size asked for, which is
	// what this is measuring, and it is a forked child of the shell, which
	// is the point of the test.
	spec := job.Spec{Args: []string{"/bin/sh", "-c",
		`dd if=/dev/zero of=/dev/null bs=200M count=20 2>/dev/null & wait`}}
	sb, err := b.Prepare(ctx, spec, 20)
	if err != nil {
		t.Fatal(err)
	}
	h, err := b.Launch(ctx, sb, spec)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Signal(ctx, h, "KILL")

	var peak int64
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		u, err := b.Sample(ctx, h)
		if err == nil && u.MemBytes > peak {
			peak = u.MemBytes
		}
		if peak > peakFloor {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	// The threshold distinguishes "the child is counted" from "only the
	// launcher is", which is what this is about. It is not the child's exact
	// 200 MiB: a containerised job's usage is sampled from the runtime at an
	// instant rather than read from the process tree, so the peak of a short
	// burst can be missed -- on a loaded machine this saw 53 MiB of a 200 MiB
	// burst. The launcher alone sits at about five megabytes, so a few tens
	// of megabytes already means the child is being seen, and the figure a
	// job is finally recorded with does not come from here anyway: it comes
	// from the kernel's own high-water mark. See Backend.Aftermath.
	if peak < peakFloor {
		t.Errorf("peak sampled memory %d MiB; the forked child was not accounted "+
			"for at all -- the launcher alone is about 5 MiB", peak>>20)
	}
	t.Logf("sampled peak %d MiB", peak>>20)
}

// peakFloor is high enough to exclude the launcher alone, low enough to
// survive a sampled measurement of a short burst.
const peakFloor = 24 << 20

func TestCleanupLeavesNoResidue(t *testing.T) {
	b, _ := newTestBackend(t)
	ctx := context.Background()
	spec := job.Spec{Args: []string{"/bin/sh", "-c", "echo x > f.txt; mkdir -p d/e; echo y > d/e/g.txt"}}
	sb, err := b.Prepare(ctx, spec, 30)
	if err != nil {
		t.Fatal(err)
	}
	h, err := b.Launch(ctx, sb, spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Wait(ctx, h); err != nil {
		t.Fatal(err)
	}

	if err := b.Cleanup(ctx, sb); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if _, err := os.Stat(sb.ScratchDir); !os.IsNotExist(err) {
		t.Error("scratch still present after cleanup")
	}
	if err := b.Cleanup(ctx, sb); err != nil { // must be idempotent
		t.Errorf("second cleanup should be a no-op, got %v", err)
	}
}

func TestCleanupRefusesPathsOutsideJobsRoot(t *testing.T) {
	b, _ := newTestBackend(t)
	outside := t.TempDir()
	sbOut := platformSandbox(outside)
	err := b.Cleanup(context.Background(), &sbOut)
	if err == nil {
		t.Fatal("cleanup must refuse a path outside the jobs root")
	}
	if _, statErr := os.Stat(outside); os.IsNotExist(statErr) {
		t.Fatal("cleanup deleted a directory outside the jobs root")
	}
}

func TestInventoryReportsHonestEnforcement(t *testing.T) {
	b, _ := newTestBackend(t)
	c, err := b.Inventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if c.MemLimit != "polled" {
		t.Errorf("MemLimit = %q; macOS memory is polled, not hard (see docs/design-notes.md)", c.MemLimit)
	}
	if c.CPULimit != "advisory" {
		t.Errorf("CPULimit = %q; macOS has no CPU quota", c.CPULimit)
	}
	if len(c.Lost) == 0 {
		t.Error("node must declare what it cannot enforce")
	}
	if c.CPUs <= 0 || c.MemBytes <= 0 {
		t.Errorf("bad inventory: cpus=%d mem=%d", c.CPUs, c.MemBytes)
	}
}

func platformSandbox(dir string) platform.Sandbox {
	return platform.Sandbox{ScratchDir: dir, JobID: 999}
}
