//go:build darwin

package darwin

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestProcessFootprintSelf(t *testing.T) {
	got, err := ProcessFootprint(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if got <= 0 {
		t.Errorf("footprint = %d, want > 0", got)
	}
}

// The regression test: a job that forks must still be accounted for.
// `taskpolicy -m` misses this case entirely, which is why polling exists.
func TestTreeFootprintCountsForkedChildren(t *testing.T) {
	// A shell that forks a child allocating ~200 MiB and holding it.
	script := `python3 -c "
import time
x = bytearray(200*1024*1024)
for i in range(0, len(x), 4096): x[i] = 1
time.sleep(10)
" &
wait`
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		syscall.Kill(-pgid, syscall.SIGKILL)
		cmd.Wait()
	}()

	// Poll until the child has faulted its pages in.
	var peak int64
	var nprocs int
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		b, n, err := TreeFootprint(pgid)
		if err != nil {
			t.Fatal(err)
		}
		if b > peak {
			peak, nprocs = b, n
		}
		if peak > 150<<20 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if peak < 150<<20 {
		t.Errorf("tree footprint peaked at %d MiB; expected >150 MiB from the forked child.\n"+
			"This is the exact case `taskpolicy -m` misses.", peak>>20)
	}
	if nprocs < 2 {
		t.Errorf("counted %d processes; expected the shell plus its child", nprocs)
	}
	t.Logf("peak tree footprint %d MiB across %d processes", peak>>20, nprocs)
}
