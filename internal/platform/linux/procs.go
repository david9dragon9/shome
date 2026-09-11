//go:build linux

package linux

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// proc tracks one launched job so Wait can be called repeatedly and from
// several goroutines.
//
// A single os/exec.Cmd may only be waited on once; the controller polls Wait
// on every reconcile tick, so the result is captured once here and replayed.
type proc struct {
	cmd  *exec.Cmd
	out  *os.File
	done chan struct{}
	code int
}

type procRegistry struct {
	mu sync.Mutex
	m  map[int64]*proc
}

func (r *procRegistry) start(id int64, cmd *exec.Cmd, out *os.File) {
	p := &proc{cmd: cmd, out: out, done: make(chan struct{})}
	r.mu.Lock()
	if r.m == nil {
		r.m = map[int64]*proc{}
	}
	r.m[id] = p
	r.mu.Unlock()

	go func() {
		err := cmd.Wait()
		p.code = 0
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				p.code = ee.ExitCode()
			} else {
				p.code = -1
			}
		}
		out.Close()
		close(p.done)
	}()
}

func (r *procRegistry) wait(ctx context.Context, id int64) (int, error) {
	r.mu.Lock()
	p := r.m[id]
	r.mu.Unlock()
	if p == nil {
		return -1, fmt.Errorf("no process record for job %d", id)
	}
	select {
	case <-p.done:
		return p.code, nil
	case <-ctx.Done():
		return -1, ctx.Err()
	}
}

// procTreeFootprint sums RSS across a process group by reading /proc.
//
// Grouped by pgid rather than by walking parent pointers: when an intermediate
// parent exits its children reparent to init and a ppid walk loses them, while
// the process group survives. Used only where no cgroup is available.
func procTreeFootprint(pgid int) (int64, int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, 0, err
	}
	var total int64
	var n int
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if statPgid(pid) != pgid {
			continue
		}
		if rss, ok := statmRSS(pid); ok {
			total += rss
			n++
		}
	}
	return total, n, nil
}

// statPgid reads the process group from /proc/<pid>/stat.
//
// The comm field can contain spaces and parentheses, so the fields after it
// are located from the LAST ')' rather than by splitting the whole line.
func statPgid(pid int) int {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return -1
	}
	s := string(b)
	i := strings.LastIndex(s, ")")
	if i < 0 || i+2 >= len(s) {
		return -1
	}
	fields := strings.Fields(s[i+2:])
	// After comm: state(0) ppid(1) pgrp(2)
	if len(fields) < 3 {
		return -1
	}
	pg, err := strconv.Atoi(fields[2])
	if err != nil {
		return -1
	}
	return pg
}

func statmRSS(pid int) (int64, bool) {
	f, err := os.Open(filepath.Join("/proc", strconv.Itoa(pid), "statm"))
	if err != nil {
		return 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return 0, false
	}
	fields := strings.Fields(sc.Text())
	if len(fields) < 2 {
		return 0, false
	}
	pages, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return 0, false
	}
	return pages * int64(os.Getpagesize()), true
}
