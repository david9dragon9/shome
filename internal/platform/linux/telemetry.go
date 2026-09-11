//go:build linux

package linux

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/davidwu/shome/internal/platform"
)

// Everything here reads /proc and /sys. No cgo, so the Linux agent still
// cross-compiles from a Mac, which is what makes one-command join work for a
// Linux machine whose owner has no Go toolchain.

// telemetryState holds the previous CPU reading, because utilisation is a rate
// between two samples. Reporting the since-boot average instead would make a
// machine that has been up for a week look permanently idle.
type telemetryState struct {
	mu   sync.Mutex
	last platform.CPUTicks
}

var telState telemetryState

// Telemetry reports what this machine is currently doing.
func (b *Backend) Telemetry(ctx context.Context) (platform.Telemetry, error) {
	now := time.Now()
	t := platform.UnknownTelemetry(now)

	if cur, ok := readProcStat(now); ok {
		telState.mu.Lock()
		pct, baseline := platform.CPURate(telState.last, cur)
		telState.last = baseline
		telState.mu.Unlock()
		t.CPUPercent = pct
	}

	if mem, ok := readMeminfo(); ok {
		if total, okT := mem["MemTotal"]; okT {
			t.MemTotalBytes = total
			// MemAvailable is the kernel's own estimate of what a new
			// allocation could get. Deriving "used" from MemFree instead
			// counts the page cache as used and makes every healthy Linux box
			// look like it is out of memory.
			if avail, okA := mem["MemAvailable"]; okA {
				t.MemUsedBytes = total - avail
			} else if free, okF := mem["MemFree"]; okF {
				t.MemUsedBytes = total - free - mem["Buffers"] - mem["Cached"]
			}
		}
		if st, okS := mem["SwapTotal"]; okS {
			t.SwapUsedBytes = st - mem["SwapFree"]
		}
	}

	if la, ok := readLoadAvg(); ok {
		t.LoadAvg1 = la
	}
	if up, ok := readUptime(); ok {
		t.Uptime = up
	}
	if pct, used, ok := readNvidiaGPU(ctx); ok {
		t.GPUPercent = pct
		t.GPUMemUsedBytes = used
	}
	// Disk, inodes and throughput. Shared with the other platform so the
	// two cannot describe the same machine differently.
	platform.FillDiskAndNet(&t, b.stateRoot(), readNetCounters(now))

	return t, nil
}

func readProcStat(now time.Time) (platform.CPUTicks, bool) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return platform.CPUTicks{}, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)[1:]
		var total, idle uint64
		for i, f := range fields {
			v, err := strconv.ParseUint(f, 10, 64)
			if err != nil {
				continue
			}
			total += v
			// Fields 3 and 4 are idle and iowait. A CPU waiting on disk is
			// not doing work, and counting iowait as busy makes a machine
			// copying a large file look saturated.
			if i == 3 || i == 4 {
				idle += v
			}
		}
		if total == 0 {
			return platform.CPUTicks{}, false
		}
		return platform.CPUTicks{Busy: total - idle, Total: total, At: now}, true
	}
	return platform.CPUTicks{}, false
}

// readMeminfo returns /proc/meminfo values in bytes.
func readMeminfo() (map[string]int64, bool) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return nil, false
	}
	defer f.Close()
	out := map[string]int64{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		key, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		v, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			continue
		}
		// meminfo is in kB except for a few entries that carry no unit.
		if len(fields) > 1 && fields[1] == "kB" {
			v *= 1024
		}
		out[key] = v
	}
	return out, len(out) > 0
}

func readLoadAvg() (float64, bool) {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, false
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	return v, err == nil
}

func readUptime() (time.Duration, bool) {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, false
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return 0, false
	}
	secs, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, false
	}
	return time.Duration(secs * float64(time.Second)), true
}

// nvidiaSMI is resolved once. Looking it up on every sample would put an exec
// lookup in the heartbeat path on machines that will never have one.
var (
	nvidiaOnce sync.Once
	nvidiaPath string
)

// readNvidiaGPU shells out to nvidia-smi.
//
// NVML would avoid the subprocess but needs cgo and the CUDA toolkit at build
// time, which would end cross-compilation for Linux agents -- a bad trade for
// a number sampled every few seconds. The timeout matters: nvidia-smi can hang
// for tens of seconds against a wedged driver, and a stuck telemetry call must
// never stall the heartbeat that keeps a node in the cluster.
func readNvidiaGPU(ctx context.Context) (percent float64, memUsed int64, ok bool) {
	nvidiaOnce.Do(func() { nvidiaPath, _ = exec.LookPath("nvidia-smi") })
	if nvidiaPath == "" {
		return 0, 0, false
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, nvidiaPath,
		"--query-gpu=utilization.gpu,memory.used",
		"--format=csv,noheader,nounits").Output()
	if err != nil {
		return 0, 0, false
	}
	// Sum across devices: the cluster schedules a node's GPU memory as a pool.
	var totalPct float64
	var n int
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		u, m, found := strings.Cut(line, ",")
		if !found {
			continue
		}
		pct, err1 := strconv.ParseFloat(strings.TrimSpace(u), 64)
		mib, err2 := strconv.ParseInt(strings.TrimSpace(m), 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		totalPct += pct
		memUsed += mib << 20
		n++
	}
	if n == 0 {
		return 0, 0, false
	}
	return clampPercent(totalPct / float64(n)), memUsed, true
}

func clampPercent(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

// Compile-time proof the backend can be used as a telemetry source; without
// it, a signature drift would silently fall back to "unknown" everywhere.
var _ platform.TelemetrySource = (*Backend)(nil)
