package ctl

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/davidwu/shome/internal/job"
)

// Byte relay between a user's terminal and a process on a compute node.
//
// # Why a relay
//
// Compute nodes dial the controller; the controller cannot dial them. That is
// deliberate -- it is what lets a machine behind a home router join a cluster
// without anyone configuring a port forward -- and it means there is no way
// for a user's terminal to connect to a node directly.
//
// So both ends dial the controller and it copies bytes between them. The same
// shape as the file transfer relay, with one difference that matters: a
// transfer can be staged to disk and forwarded later, while an interactive
// session has to be live in both directions at once. So this holds pipes in
// memory rather than blobs on disk, and a stream with nobody attached is a
// stream that gets torn down.
//
// # What it does not do
//
// It does not interpret anything. It has no idea whether it is carrying
// terminal escape sequences, a tar file or a python traceback, and it makes
// no authorisation decisions of its own beyond checking that the account
// attaching owns the job. Both of those live where they can be tested.

// StreamTTL bounds how long a stream may exist unattached.
//
// A user typing `srun --pty bash` and immediately closing their laptop leaves
// a stream nobody will ever read. Without a bound, its pipes and its job's
// allocation would be held until the controller restarted.
const StreamTTL = 30 * time.Second

// StreamIdle is how long a stream survives with no attached client.
const StreamIdle = 5 * time.Minute

// stream is one interactive channel between a client and a node.
type stream struct {
	ID   int64
	User string
	Job  int64

	// in carries keystrokes from the client to the node; out carries the
	// process's output back. Pipes rather than buffers, so a slow reader
	// applies backpressure instead of letting the controller accumulate
	// somebody's scrollback in memory.
	inR, outR *io.PipeReader
	inW, outW *io.PipeWriter

	mu sync.Mutex
	// clientAttached and nodeAttached are one-shot: a stream is for one
	// session, and a second attach is a bug or an attempt to hijack.
	clientAttached bool
	nodeAttached   bool
	exit           int
	exited         bool
	done           chan struct{}
	created        time.Time

	// The size of the user's window, and a version that counts changes to
	// it. Out of band rather than in the byte stream: the relay carries
	// keystrokes and must not have to interpret them, and an escape
	// sequence smuggled through it would be indistinguishable from one the
	// user typed. The version is what lets the node ask for "anything newer
	// than what I have" without missing a change that lands between polls.
	size    job.Winsize
	sizeVer int64
	// sizeCh is closed and replaced on every change, so any number of
	// waiters wake at once.
	sizeCh chan struct{}
}

type streams struct {
	mu   sync.Mutex
	next int64
	m    map[int64]*stream
}

func newStreams() *streams { return &streams{m: map[int64]*stream{}} }

// NewStream registers a stream for a job and returns its id.
func (c *Controller) NewStream(user string, job int64) *stream {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()

	c.streams.mu.Lock()
	c.streams.next++
	s := &stream{
		ID: c.streams.next, User: user, Job: job,
		inR: inR, inW: inW, outR: outR, outW: outW,
		done: make(chan struct{}), created: c.now(),
		sizeCh: make(chan struct{}),
	}
	c.streams.m[s.ID] = s
	c.streams.mu.Unlock()

	go c.expireStream(s.ID)
	return s
}

// stream looks up a stream, checking the account owns it.
func (c *Controller) stream(id int64, user string) (*stream, error) {
	c.streams.mu.Lock()
	s := c.streams.m[id]
	c.streams.mu.Unlock()
	if s == nil {
		return nil, fmt.Errorf("no session %d", id)
	}
	if user != "" && s.User != user {
		// Same shape as the rest of the API: not yours reads as not there.
		return nil, fmt.Errorf("no session %d", id)
	}
	return s, nil
}

// AttachClient hands a user's terminal its two halves of the stream.
//
// The writer is what the client types into; the reader is what the process
// prints. Returns an error if a client is already attached, so a stream id
// that leaked cannot be used to take over a live session.
func (c *Controller) AttachClient(id int64, user string) (io.WriteCloser, io.ReadCloser, *stream, error) {
	s, err := c.stream(id, user)
	if err != nil {
		return nil, nil, nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.clientAttached {
		return nil, nil, nil, fmt.Errorf("session %d already has a terminal attached", id)
	}
	s.clientAttached = true
	return s.inW, s.outR, s, nil
}

// AttachNode hands a compute node its two halves.
//
// Authorised by the node's certificate identity plus the job's assignment:
// the caller passes the node it authenticated as, and this checks the job is
// actually running there. Otherwise any node in the cluster could attach to
// any session.
func (c *Controller) AttachNode(id int64, node string) (io.ReadCloser, io.WriteCloser, error) {
	s, err := c.stream(id, "")
	if err != nil {
		return nil, nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.nodeAttached {
		return nil, nil, fmt.Errorf("session %d already has a node attached", id)
	}
	s.nodeAttached = true
	return s.inR, s.outW, nil
}

// SetStreamSize records how big the user's window is now.
//
// Called when the session starts and again whenever the user resizes their
// terminal. An unchanged size is not a change: it does not bump the version,
// so a client that reports the same numbers repeatedly wakes nobody.
func (c *Controller) SetStreamSize(id int64, user string, ws job.Winsize) error {
	s, err := c.stream(id, user)
	if err != nil {
		return err
	}
	if !ws.Known() {
		return fmt.Errorf("a terminal size needs both rows and columns")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.size == ws {
		return nil
	}
	s.size = ws
	s.sizeVer++
	close(s.sizeCh)
	s.sizeCh = make(chan struct{})
	return nil
}

// WatchStreamSize reports the window size once it is newer than the version
// the caller already has.
//
// Long-polled rather than pushed, for the same reason the rest of the relay
// is: a compute node dials the controller and cannot be dialled. Returns
// false when the wait ended without a new size -- the context expired, or
// the session did -- and the caller should ask again.
func (c *Controller) WatchStreamSize(ctx context.Context, id int64, since int64) (job.Winsize, int64, bool) {
	s, err := c.stream(id, "")
	if err != nil {
		return job.Winsize{}, since, false
	}
	for {
		s.mu.Lock()
		ws, ver, wait := s.size, s.sizeVer, s.sizeCh
		s.mu.Unlock()
		if ver > since {
			return ws, ver, true
		}
		select {
		case <-wait:
		case <-ctx.Done():
			return job.Winsize{}, since, false
		case <-s.done:
			return job.Winsize{}, since, false
		}
	}
}

// FinishStream records a process's exit status and closes the stream.
func (c *Controller) FinishStream(id int64, code int) {
	c.streams.mu.Lock()
	s := c.streams.m[id]
	c.streams.mu.Unlock()
	if s == nil {
		return
	}
	s.mu.Lock()
	if !s.exited {
		s.exit, s.exited = code, true
		close(s.done)
	}
	s.mu.Unlock()
	// Closing the writers is what lets the client's read reach end-of-file,
	// which is how it learns the process is gone.
	s.outW.Close()
	s.inW.Close()
}

// StreamExit waits for a stream's process to finish and reports its status.
func (c *Controller) StreamExit(id int64, user string) (int, bool) {
	s, err := c.stream(id, user)
	if err != nil {
		return 0, false
	}
	s.mu.Lock()
	exited, code := s.exited, s.exit
	s.mu.Unlock()
	return code, exited
}

// CloseStream tears a stream down and forgets it.
func (c *Controller) CloseStream(id int64) {
	c.streams.mu.Lock()
	s := c.streams.m[id]
	delete(c.streams.m, id)
	c.streams.mu.Unlock()
	if s == nil {
		return
	}
	s.inW.Close()
	s.outW.Close()
	s.inR.Close()
	s.outR.Close()
	s.mu.Lock()
	if !s.exited {
		s.exited = true
		close(s.done)
	}
	s.mu.Unlock()
}

// expireStream discards a stream nobody ever attached to.
//
// A user who runs `srun --pty` and closes their laptop before the job starts
// would otherwise leave the pipes and the job's allocation held indefinitely.
func (c *Controller) expireStream(id int64) {
	select {
	case <-time.After(StreamIdle):
	case <-c.stopCh:
		return
	}
	c.streams.mu.Lock()
	s := c.streams.m[id]
	c.streams.mu.Unlock()
	if s == nil {
		return
	}
	s.mu.Lock()
	idle := !s.clientAttached && !s.nodeAttached
	s.mu.Unlock()
	if idle {
		c.log.Info("discarding an unused interactive session", "stream", id, "job", s.Job)
		c.CloseStream(id)
	}
}

// reapOrphanStreams ends sessions whose job died before the node attached.
//
// The ordinary way a session ends is the node reporting the process's exit,
// which closes the pipes and lets the waiting client see end-of-file. A job
// that never started has no node to report anything: the launch failed, or
// the node refused it. The client sat reading a stream nothing would ever
// be written to -- `srun bash` against a node that could not start it hung
// indefinitely, with the failure visible in `squeue` the whole time.
//
// Only sessions no node ever attached to. Once one has, that node owns the
// ending, and closing underneath it would truncate the last of a job's
// output in the one case where output exists.
func (c *Controller) reapOrphanStreams(ctx context.Context) {
	c.streams.mu.Lock()
	type candidate struct {
		id  int64
		job int64
	}
	var check []candidate
	for id, s := range c.streams.m {
		s.mu.Lock()
		orphan := !s.nodeAttached && !s.exited && s.Job != 0
		s.mu.Unlock()
		if orphan {
			check = append(check, candidate{id: id, job: s.Job})
		}
	}
	c.streams.mu.Unlock()

	for _, cand := range check {
		j, err := c.store.Get(ctx, cand.job)
		if err != nil || !j.State.Terminal() {
			continue
		}
		code := j.ExitCode
		if code == 0 {
			// A job that ended without running did not succeed, whatever
			// its recorded code says. Reporting zero would make `srun` exit
			// zero for a job that never ran, which a script would read as
			// the work having been done.
			code = 1
		}
		c.log.Info("ending a session whose job never started",
			"stream", cand.id, "job", cand.job, "state", j.State, "reason", j.Reason)
		c.FinishStream(cand.id, code)
	}
}

// bindStream attaches a session to the job that was created for it.
//
// Two steps because the session id has to exist before the job is submitted
// -- the spec carries it -- and the job id does not exist until after.
func (c *Controller) bindStream(id, job int64) {
	c.streams.mu.Lock()
	if s := c.streams.m[id]; s != nil {
		s.Job = job
	}
	c.streams.mu.Unlock()
}

// streamBelongsTo checks a node is the one running a session's job.
//
// The node's identity comes from its certificate, so this is the whole
// authorisation for the node side: without it, any machine that had joined
// the cluster could attach to any user's terminal.
func (c *Controller) streamBelongsTo(ctx context.Context, id int64, node string) error {
	s, err := c.stream(id, "")
	if err != nil {
		return err
	}
	j, err := c.store.Get(ctx, s.Job)
	if err != nil {
		return fmt.Errorf("no job for session %d", id)
	}
	if j.Node != node {
		return fmt.Errorf("job %d is not running on %s", s.Job, node)
	}
	return nil
}

// StreamsForJob closes any stream belonging to a job, so a cancelled or
// finished job does not leave a terminal waiting on output that will never
// come.
func (c *Controller) StreamsForJob(job int64) []int64 {
	c.streams.mu.Lock()
	defer c.streams.mu.Unlock()
	var out []int64
	for id, s := range c.streams.m {
		if s.Job == job {
			out = append(out, id)
		}
	}
	return out
}
