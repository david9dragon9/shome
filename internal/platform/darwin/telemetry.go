//go:build darwin && cgo

package darwin

/*
#include <mach/mach.h>
#include <mach/mach_host.h>
#include <sys/sysctl.h>
#include <stdlib.h>
#include <stdio.h>

// shome_cpu_ticks returns cumulative CPU tick counters across all cores.
// Utilisation is the ratio of busy to total ticks between two samples --
// an instantaneous reading is not a thing the kernel offers.
static int shome_cpu_ticks(unsigned long long *user, unsigned long long *system,
                           unsigned long long *idle, unsigned long long *nice) {
    host_cpu_load_info_data_t info;
    mach_msg_type_number_t count = HOST_CPU_LOAD_INFO_COUNT;
    if (host_statistics(mach_host_self(), HOST_CPU_LOAD_INFO,
                        (host_info_t)&info, &count) != KERN_SUCCESS) {
        return -1;
    }
    *user   = info.cpu_ticks[CPU_STATE_USER];
    *system = info.cpu_ticks[CPU_STATE_SYSTEM];
    *idle   = info.cpu_ticks[CPU_STATE_IDLE];
    *nice   = info.cpu_ticks[CPU_STATE_NICE];
    return 0;
}

// shome_vm_stats reports physical memory use.
//
// "Used" on macOS is not a single number the kernel hands out. This follows
// what Activity Monitor calls memory used: anonymous pages plus wired plus
// compressed. Inactive and purgeable pages are excluded because the system
// reclaims them on demand -- counting them makes an idle Mac look full, which
// is the classic way to misread macOS memory.
static int shome_vm_stats(unsigned long long *used, unsigned long long *total,
                          unsigned long long *page_size) {
    mach_port_t host = mach_host_self();
    vm_size_t pgsz = 0;
    if (host_page_size(host, &pgsz) != KERN_SUCCESS) return -1;

    vm_statistics64_data_t vm;
    mach_msg_type_number_t count = HOST_VM_INFO64_COUNT;
    if (host_statistics64(host, HOST_VM_INFO64, (host_info64_t)&vm, &count) != KERN_SUCCESS) {
        return -1;
    }
    unsigned long long anonymous = (unsigned long long)vm.internal_page_count
                                 - (unsigned long long)vm.purgeable_count;
    *used = (anonymous + (unsigned long long)vm.wire_count
                       + (unsigned long long)vm.compressor_page_count) * (unsigned long long)pgsz;
    *page_size = (unsigned long long)pgsz;

    int64_t memsize = 0;
    size_t len = sizeof(memsize);
    if (sysctlbyname("hw.memsize", &memsize, &len, NULL, 0) != 0) return -1;
    *total = (unsigned long long)memsize;
    return 0;
}

static int shome_swap_used(unsigned long long *used) {
    struct xsw_usage sw;
    size_t len = sizeof(sw);
    if (sysctlbyname("vm.swapusage", &sw, &len, NULL, 0) != 0) return -1;
    *used = (unsigned long long)sw.xsu_used;
    return 0;
}

static int shome_loadavg(double *one) {
    double avg[3];
    if (getloadavg(avg, 3) < 1) return -1;
    *one = avg[0];
    return 0;
}

static int shome_boottime(long long *secs) {
    struct timeval tv;
    size_t len = sizeof(tv);
    if (sysctlbyname("kern.boottime", &tv, &len, NULL, 0) != 0) return -1;
    *secs = (long long)tv.tv_sec;
    return 0;
}
*/
import "C"

import (
	"context"
	"sync"
	"time"

	"github.com/davidwu/shome/internal/platform"
)

// telemetryState remembers the previous CPU reading.
//
// CPU utilisation is a rate, so it needs two samples. Keeping the last one
// here means the first call after startup reports Unknown rather than a
// meaningless average since boot -- which on a machine that has been up for a
// week is indistinguishable from "idle" no matter how busy it is now.
type telemetryState struct {
	mu   sync.Mutex
	last platform.CPUTicks
}

var telState telemetryState

// Telemetry reports what this machine is currently doing.
func (b *Backend) Telemetry(ctx context.Context) (platform.Telemetry, error) {
	now := time.Now()
	t := platform.UnknownTelemetry(now)

	if cur, ok := readCPUTicks(now); ok {
		telState.mu.Lock()
		pct, baseline := platform.CPURate(telState.last, cur)
		telState.last = baseline
		telState.mu.Unlock()
		t.CPUPercent = pct
	}

	var used, total, pgsz C.ulonglong
	if C.shome_vm_stats(&used, &total, &pgsz) == 0 {
		t.MemUsedBytes = int64(used)
		t.MemTotalBytes = int64(total)
	}
	var swap C.ulonglong
	if C.shome_swap_used(&swap) == 0 {
		t.SwapUsedBytes = int64(swap)
	}
	var la C.double
	if C.shome_loadavg(&la) == 0 {
		t.LoadAvg1 = float64(la)
	}
	var boot C.longlong
	if C.shome_boottime(&boot) == 0 && boot > 0 {
		t.Uptime = now.Sub(time.Unix(int64(boot), 0))
	}

	// Thermal, battery and idle are deliberately not read here. The agent
	// already samples them for the owner-yield policy and overlays them onto
	// this reading, so the dashboard and the policy engine cannot disagree
	// about whether someone is sitting at the machine.

	// GPU utilisation is deliberately left Unknown. macOS exposes no public
	// API for it, and the alternative -- reporting scheduler allocation as if
	// it were measured load -- would be wrong exactly when someone is trying
	// to find out whether the GPU is actually busy.
	// Disk, inodes and throughput. Shared with the other platform so the
	// two cannot describe the same machine differently.
	platform.FillDiskAndNet(&t, b.stateRoot(), readNetCounters(now))

	return t, nil
}

func readCPUTicks(now time.Time) (platform.CPUTicks, bool) {
	var user, system, idle, nice C.ulonglong
	if C.shome_cpu_ticks(&user, &system, &idle, &nice) != 0 {
		return platform.CPUTicks{}, false
	}
	busy := uint64(user) + uint64(system) + uint64(nice)
	return platform.CPUTicks{Busy: busy, Total: busy + uint64(idle), At: now}, true
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

var _ platform.TelemetrySource = (*Backend)(nil)
