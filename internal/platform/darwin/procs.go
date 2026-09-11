//go:build darwin

package darwin

import (
	"os"
	"os/exec"
	"sync"
)

// proc tracks a launched job. Exactly one goroutine calls cmd.Wait(); everyone
// else observes the result through done. Calling Wait twice on an exec.Cmd is a
// race, so Backend.Wait must not reap the process itself.
type proc struct {
	cmd  *exec.Cmd
	out  *os.File
	done chan struct{}
	code int
	// container is the container id when this job runs in one. Signalling
	// the host process does not stop a container, so it has to be named.
	container string
}

type procRegistry struct {
	mu sync.Mutex
	m  map[int64]*proc
}

var procs = &procRegistry{m: map[int64]*proc{}}

func (r *procRegistry) put(id int64, p *proc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[id] = p
}

func (r *procRegistry) get(id int64) *proc {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.m[id]
}

func (r *procRegistry) del(id int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.m, id)
}

// reap is the single owner of cmd.Wait() for this job.
func (p *proc) reap() {
	err := p.cmd.Wait()
	p.code = -1
	if p.cmd.ProcessState != nil {
		p.code = p.cmd.ProcessState.ExitCode()
	} else if err == nil {
		p.code = 0
	}
	if p.out != nil {
		p.out.Close()
	}
	close(p.done)
}
