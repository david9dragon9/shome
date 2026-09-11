//go:build darwin

package darwin

/*
#include <libproc.h>
#include <sys/proc_info.h>
#include <stdlib.h>

// Wrapper: cgo cannot express the rusage_info_t union cast cleanly.
static int shome_footprint(int pid, uint64_t *out) {
    struct rusage_info_v2 ri;
    int rc = proc_pid_rusage(pid, RUSAGE_INFO_V2, (rusage_info_t *)&ri);
    if (rc != 0) return rc;
    *out = ri.ri_phys_footprint;
    return 0;
}

static int shome_pgid(int pid, int *pgid) {
    struct proc_bsdinfo bi;
    int n = proc_pidinfo(pid, PROC_PIDTBSDINFO, 0, &bi, sizeof(bi));
    if (n != (int)sizeof(bi)) return -1;
    *pgid = (int)bi.pbi_pgid;
    return 0;
}
*/
import "C"

import (
	"fmt"
	"unsafe"
)

// TreeFootprint sums ri_phys_footprint over every process in process group
// pgid, and reports how many processes contributed.
//
// This is macOS memory *enforcement*, not telemetry. `taskpolicy -m` is not
// inherited and does not survive execve (see docs/design-notes.md), so a batch script -- which is
// a shell that forks -- would escape a kernel limit entirely. Polling the group
// is what actually holds.
//
// Grouping is by process group rather than by walking parent pointers: when an
// intermediate parent exits, its children reparent to launchd and a ppid walk
// loses them, whereas the pgid survives.
//
// ri_phys_footprint is the same metric jetsam uses, so the number here matches
// what the kernel would act on.
func TreeFootprint(pgid int) (bytes int64, nprocs int, err error) {
	// Size the pid buffer, then over-allocate: the process table can grow
	// between the two calls.
	n := C.proc_listpids(C.PROC_ALL_PIDS, 0, nil, 0)
	if n <= 0 {
		return 0, 0, fmt.Errorf("proc_listpids: cannot size pid buffer")
	}
	cap := int(n)/int(unsafe.Sizeof(C.int(0))) + 64
	pids := make([]C.int, cap)
	got := C.proc_listpids(C.PROC_ALL_PIDS, 0, unsafe.Pointer(&pids[0]),
		C.int(cap*int(unsafe.Sizeof(C.int(0)))))
	if got <= 0 {
		return 0, 0, fmt.Errorf("proc_listpids: enumeration failed")
	}
	count := int(got) / int(unsafe.Sizeof(C.int(0)))

	for i := 0; i < count; i++ {
		pid := pids[i]
		if pid <= 0 {
			continue
		}
		var pg C.int
		if C.shome_pgid(pid, &pg) != 0 {
			continue // exited, or not ours to inspect
		}
		if int(pg) != pgid {
			continue
		}
		var fp C.uint64_t
		if C.shome_footprint(pid, &fp) != 0 {
			continue // raced with exit
		}
		bytes += int64(fp)
		nprocs++
	}
	return bytes, nprocs, nil
}

// ProcessFootprint returns one process's phys_footprint.
func ProcessFootprint(pid int) (int64, error) {
	var fp C.uint64_t
	if rc := C.shome_footprint(C.int(pid), &fp); rc != 0 {
		return 0, fmt.Errorf("proc_pid_rusage(%d): rc=%d", pid, int(rc))
	}
	return int64(fp), nil
}

// TreePIDs lists every live pid in a job's process group.
//
// Same grouping rationale as TreeFootprint: an intermediate parent exiting
// reparents its children to launchd, so a ppid walk loses them while the pgid
// survives.
func TreePIDs(pgid int) ([]int, error) {
	n := C.proc_listpids(C.PROC_ALL_PIDS, 0, nil, 0)
	if n <= 0 {
		return nil, fmt.Errorf("proc_listpids: cannot size pid buffer")
	}
	capacity := int(n)/int(unsafe.Sizeof(C.int(0))) + 64
	pids := make([]C.int, capacity)
	got := C.proc_listpids(C.PROC_ALL_PIDS, 0, unsafe.Pointer(&pids[0]),
		C.int(capacity*int(unsafe.Sizeof(C.int(0)))))
	if got <= 0 {
		return nil, fmt.Errorf("proc_listpids: enumeration failed")
	}
	count := int(got) / int(unsafe.Sizeof(C.int(0)))
	var out []int
	for i := 0; i < count; i++ {
		pid := pids[i]
		if pid <= 0 {
			continue
		}
		var pg C.int
		if C.shome_pgid(pid, &pg) != 0 || int(pg) != pgid {
			continue
		}
		out = append(out, int(pid))
	}
	return out, nil
}
