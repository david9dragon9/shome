//go:build darwin && cgo

package darwin

import (
	"context"

	"github.com/davidwu/shome/internal/platform"
	"os"
	"testing"
	"time"
)

func TestTelemetryReadsRealMachine(t *testing.T) {
	b := New(t.TempDir(), os.Getenv("HOME"))
	ctx := context.Background()

	// First call has no previous sample, so CPU is legitimately unknown.
	first, err := b.Telemetry(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first.KnownCPU() {
		t.Errorf("first sample reported CPU %.1f%%; a rate needs two readings", first.CPUPercent)
	}
	if first.MemTotalBytes <= 0 {
		t.Fatalf("no total memory reading: %d", first.MemTotalBytes)
	}
	if first.MemUsedBytes <= 0 || first.MemUsedBytes > first.MemTotalBytes {
		t.Errorf("memory used %d is not within 0..%d", first.MemUsedBytes, first.MemTotalBytes)
	}
	if first.Uptime <= 0 {
		t.Errorf("uptime %v", first.Uptime)
	}
	if first.LoadAvg1 < 0 {
		t.Errorf("load average %v", first.LoadAvg1)
	}
	// GPU utilisation has no public API on macOS and must stay unknown rather
	// than being faked from allocation.
	if first.KnownGPU() {
		t.Errorf("GPU utilisation reported as %.1f; macOS cannot measure it", first.GPUPercent)
	}

	// Poll until a reading appears rather than sleeping a fixed interval and
	// hoping. The kernel's tick counters advance coarsely, so how long this
	// takes depends on the machine and on how loaded it is -- a fixed sleep
	// makes the test flaky on exactly the busy machines it matters on.
	var second platform.Telemetry
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		second, err = b.Telemetry(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if second.KnownCPU() {
			break
		}
	}
	if !second.KnownCPU() {
		t.Fatal("no CPU reading after 5s of polling")
	}
	if second.CPUPercent < 0 || second.CPUPercent > 100 {
		t.Errorf("CPU %.1f%% is outside 0..100 -- it must be normalised across cores", second.CPUPercent)
	}
	t.Logf("cpu=%.1f%% load=%.2f mem=%d/%d MiB swap=%d MiB uptime=%s",
		second.CPUPercent, second.LoadAvg1,
		second.MemUsedBytes>>20, second.MemTotalBytes>>20,
		second.SwapUsedBytes>>20, second.Uptime.Round(time.Minute))
}
