package agent

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/davidwu/shome/internal/job"
	"github.com/davidwu/shome/internal/ptyx"
)

// sizeRelay is a controller that reports one window size and then has
// nothing further to say, which is what a session whose window is left alone
// looks like.
type sizeRelay struct {
	sizes []job.Winsize
	asked chan int64
}

func (r *sizeRelay) StreamOut(context.Context, int64, io.Reader) error { return nil }

func (r *sizeRelay) StreamIn(context.Context, int64) (io.ReadCloser, error) {
	return io.NopCloser(&blockingReader{}), nil
}

func (r *sizeRelay) StreamExit(context.Context, int64, int) error { return nil }

func (r *sizeRelay) StreamSize(ctx context.Context, stream int64, since int64) (job.Winsize, int64, error) {
	select {
	case r.asked <- since:
	default:
	}
	if int(since) < len(r.sizes) {
		return r.sizes[since], since + 1, nil
	}
	// Nothing new: the same version back, as a timed-out long poll does.
	<-ctx.Done()
	return job.Winsize{}, since, ctx.Err()
}

// blockingReader never returns, standing in for a user who types nothing.
type blockingReader struct{}

func (b *blockingReader) Read([]byte) (int, error) { select {} }

// The size the user reports has to end up on the job's terminal. Without
// this the shell wraps at whatever width the terminal was created with, and
// the user's window has nothing to do with it.
func TestFollowSizeResizesTheTerminal(t *testing.T) {
	p, err := ptyx.Open()
	if err != nil {
		t.Skipf("no pseudo-terminal available: %v", err)
	}
	defer p.Close()
	p.Resize(job.DefaultWinsize.Rows, job.DefaultWinsize.Cols, 0, 0)

	a := &Agent{Log: slog.New(slog.DiscardHandler)}
	rel := &sizeRelay{
		sizes: []job.Winsize{{Rows: 50, Cols: 203}, {Rows: 62, Cols: 190}},
		asked: make(chan int64, 8),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	defer close(done)
	go a.followSize(ctx, rel, 1, 1, p, done)

	want := rel.sizes[len(rel.sizes)-1]
	deadline := time.Now().Add(3 * time.Second)
	for {
		ws, err := unix.IoctlGetWinsize(int(p.Slave.Fd()), unix.TIOCGWINSZ)
		if err != nil {
			t.Fatalf("read the terminal's size: %v", err)
		}
		if ws.Row == want.Rows && ws.Col == want.Cols {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("terminal is %dx%d, want %dx%d", ws.Row, ws.Col, want.Rows, want.Cols)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A controller that answers "nothing new" at once -- a session it has
// already forgotten, say -- must not turn the size loop into a spin. The
// long poll is what paces this loop, so when it stops pacing, the loop has
// to pace itself.
type instantRelay struct {
	mu    sync.Mutex
	calls int
	stop  chan struct{}
}

func (r *instantRelay) StreamOut(context.Context, int64, io.Reader) error { return nil }

func (r *instantRelay) StreamIn(context.Context, int64) (io.ReadCloser, error) {
	return io.NopCloser(&blockingReader{}), nil
}

func (r *instantRelay) StreamExit(context.Context, int64, int) error { return nil }

func (r *instantRelay) StreamSize(_ context.Context, _ int64, since int64) (job.Winsize, int64, error) {
	r.mu.Lock()
	r.calls++
	r.mu.Unlock()
	return job.Winsize{}, since, nil
}

func TestFollowSizeDoesNotSpinWhenNothingIsNew(t *testing.T) {
	p, err := ptyx.Open()
	if err != nil {
		t.Skipf("no pseudo-terminal available: %v", err)
	}
	defer p.Close()

	a := &Agent{Log: slog.New(slog.DiscardHandler)}
	rel := &instantRelay{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go a.followSize(ctx, rel, 1, 1, p, done)

	time.Sleep(300 * time.Millisecond)
	close(done)
	rel.mu.Lock()
	calls := rel.calls
	rel.mu.Unlock()
	// One immediately, and then it waits: anything more than a couple means
	// it is asking as fast as the controller can answer.
	if calls > 2 {
		t.Errorf("asked for the window size %d times in 300ms", calls)
	}
}

// The loop has to end with the job. Nothing else stops it: the poll returns
// forever, and the context outlives one job.
func TestFollowSizeStopsWhenTheJobDoes(t *testing.T) {
	p, err := ptyx.Open()
	if err != nil {
		t.Skipf("no pseudo-terminal available: %v", err)
	}
	defer p.Close()

	a := &Agent{Log: slog.New(slog.DiscardHandler)}
	rel := &sizeRelay{asked: make(chan int64, 8)}
	done := make(chan struct{})
	ended := make(chan struct{})
	go func() {
		a.followSize(context.Background(), rel, 1, 1, p, done)
		close(ended)
	}()
	<-rel.asked
	close(done)
	select {
	case <-ended:
	case <-time.After(3 * time.Second):
		t.Error("the size loop outlived the job")
	}
}
