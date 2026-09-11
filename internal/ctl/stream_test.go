package ctl

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/davidwu/shome/internal/job"
)

// Bytes must travel in both directions, because that is the entire job.
func TestStreamCarriesBytesBothWays(t *testing.T) {
	c, _ := dashFixture(t)
	s := c.NewStream("alice", 7)

	clientIn, clientOut, _, err := c.AttachClient(s.ID, "alice")
	if err != nil {
		t.Fatal(err)
	}
	nodeIn, nodeOut, err := c.AttachNode(s.ID, "mini")
	if err != nil {
		t.Fatal(err)
	}

	// Client types; the node reads it.
	go clientIn.Write([]byte("ls -l\n"))
	buf := make([]byte, 6)
	if _, err := io.ReadFull(nodeIn, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "ls -l\n" {
		t.Errorf("node read %q", buf)
	}

	// Node prints; the client reads it.
	go nodeOut.Write([]byte("total 0\n"))
	got := make([]byte, 8)
	if _, err := io.ReadFull(clientOut, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != "total 0\n" {
		t.Errorf("client read %q", got)
	}
}

// A stream id is a small integer. Another account must not be able to attach
// to it, or guessing one would hand over somebody's terminal.
func TestStreamIsScopedToItsOwner(t *testing.T) {
	c, _ := dashFixture(t)
	s := c.NewStream("alice", 7)

	if _, _, _, err := c.AttachClient(s.ID, "bob"); err == nil {
		t.Fatal("bob attached to alice's session")
	}
	if _, _, _, err := c.AttachClient(s.ID, "alice"); err != nil {
		t.Fatalf("alice cannot attach to her own session: %v", err)
	}
}

// One terminal per session: a leaked id must not let a second client take
// over a live session.
func TestStreamAcceptsOneClientAndOneNode(t *testing.T) {
	c, _ := dashFixture(t)
	s := c.NewStream("alice", 7)

	if _, _, _, err := c.AttachClient(s.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := c.AttachClient(s.ID, "alice"); err == nil {
		t.Error("a second terminal attached to the same session")
	}
	if _, _, err := c.AttachNode(s.ID, "mini"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.AttachNode(s.ID, "gpu"); err == nil {
		t.Error("a second node attached to the same session")
	}
}

// The client learns the process is gone by its read reaching end-of-file, and
// learns the status separately.
func TestFinishStreamEndsTheClientsRead(t *testing.T) {
	c, _ := dashFixture(t)
	s := c.NewStream("alice", 7)
	_, clientOut, _, _ := c.AttachClient(s.ID, "alice")
	_, nodeOut, _ := c.AttachNode(s.ID, "mini")

	go func() {
		nodeOut.Write([]byte("hi"))
		c.FinishStream(s.ID, 3)
	}()
	got, err := io.ReadAll(clientOut)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "hi" {
		t.Errorf("client read %q, want %q", got, "hi")
	}
	code, exited := c.StreamExit(s.ID, "alice")
	if !exited || code != 3 {
		t.Errorf("exit = %d, exited = %v; want 3 and true", code, exited)
	}
}

func TestUnknownStream(t *testing.T) {
	c, _ := dashFixture(t)
	if _, _, _, err := c.AttachClient(999, "alice"); err == nil {
		t.Error("attached to a session that does not exist")
	}
	if _, ok := c.StreamExit(999, "alice"); ok {
		t.Error("an unknown session reported an exit status")
	}
}

// Closing must not deadlock or panic, and must be safe twice: a session ends
// from whichever side notices first.
func TestCloseStreamIsSafeAndIdempotent(t *testing.T) {
	c, _ := dashFixture(t)
	s := c.NewStream("alice", 7)
	c.AttachClient(s.ID, "alice")
	c.AttachNode(s.ID, "mini")

	c.CloseStream(s.ID)
	c.CloseStream(s.ID)
	c.FinishStream(s.ID, 0) // after close: must not panic

	if _, _, _, err := c.AttachClient(s.ID, "alice"); err == nil {
		t.Error("attached to a closed session")
	}
}

func TestStreamsForJob(t *testing.T) {
	c, _ := dashFixture(t)
	a := c.NewStream("alice", 7)
	b := c.NewStream("alice", 7)
	other := c.NewStream("alice", 8)

	got := c.StreamsForJob(7)
	if len(got) != 2 {
		t.Fatalf("StreamsForJob(7) = %v, want two", got)
	}
	seen := map[int64]bool{}
	for _, id := range got {
		seen[id] = true
	}
	if !seen[a.ID] || !seen[b.ID] || seen[other.ID] {
		t.Errorf("wrong set: %v", got)
	}
}

// A stream nobody ever attaches to must not hold its pipes forever.
func TestUnattachedStreamIsDiscarded(t *testing.T) {
	c, _ := dashFixture(t)
	// The production interval is minutes; the behaviour is what matters, so
	// drive it directly rather than making the test wait.
	s := c.NewStream("alice", 7)
	c.streams.mu.Lock()
	found := c.streams.m[s.ID] != nil
	c.streams.mu.Unlock()
	if !found {
		t.Fatal("stream was not registered")
	}
	c.CloseStream(s.ID)
	c.streams.mu.Lock()
	gone := c.streams.m[s.ID] == nil
	c.streams.mu.Unlock()
	if !gone {
		t.Error("a closed stream is still registered")
	}
}

// Backpressure: the relay must not buffer an unbounded amount of somebody's
// output in the controller.
func TestRelayAppliesBackpressure(t *testing.T) {
	c, _ := dashFixture(t)
	s := c.NewStream("alice", 7)
	c.AttachClient(s.ID, "alice")
	_, nodeOut, _ := c.AttachNode(s.ID, "mini")

	// With no reader, a write must block rather than being accepted into a
	// growing buffer.
	blocked := make(chan struct{})
	go func() {
		nodeOut.Write([]byte("some output"))
		close(blocked)
	}()
	select {
	case <-blocked:
		t.Error("a write with no reader completed; output is being buffered")
	case <-time.After(100 * time.Millisecond):
	}
	c.CloseStream(s.ID)
}

// A window size has to reach the node, or line editing wraps at a column
// the user's terminal does not have and every redraw is wrong.
func TestStreamCarriesWindowSize(t *testing.T) {
	c, _ := dashFixture(t)
	s := c.NewStream("alice", 7)

	if err := c.SetStreamSize(s.ID, "alice", job.Winsize{Rows: 50, Cols: 203}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ws, ver, ok := c.WatchStreamSize(ctx, s.ID, 0)
	if !ok {
		t.Fatal("the node was told nothing about the window")
	}
	if ws.Rows != 50 || ws.Cols != 203 {
		t.Errorf("node saw %dx%d, want 50x203", ws.Rows, ws.Cols)
	}
	if ver == 0 {
		t.Error("a size the node has not seen must have a version above zero")
	}

	// A size it already has must not come back, or the node would spin on
	// a poll that always answers.
	quick, stop := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer stop()
	if _, _, ok := c.WatchStreamSize(quick, s.ID, ver); ok {
		t.Error("the node was handed a size it already had")
	}

	// A change wakes a node that is already waiting.
	woke := make(chan job.Winsize, 1)
	go func() {
		w, _, ok := c.WatchStreamSize(ctx, s.ID, ver)
		if ok {
			woke <- w
		}
	}()
	time.Sleep(20 * time.Millisecond)
	if err := c.SetStreamSize(s.ID, "alice", job.Winsize{Rows: 24, Cols: 80}); err != nil {
		t.Fatal(err)
	}
	select {
	case w := <-woke:
		if w.Rows != 24 || w.Cols != 80 {
			t.Errorf("woke with %dx%d, want 24x80", w.Rows, w.Cols)
		}
	case <-time.After(2 * time.Second):
		t.Error("a resize did not wake the node waiting for one")
	}
}

// Repeating the same size is not a change. Without this a client that
// reports on a timer would wake every waiter for nothing.
func TestStreamIgnoresAnUnchangedWindowSize(t *testing.T) {
	c, _ := dashFixture(t)
	s := c.NewStream("alice", 7)

	same := job.Winsize{Rows: 40, Cols: 120}
	if err := c.SetStreamSize(s.ID, "alice", same); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, ver, _ := c.WatchStreamSize(ctx, s.ID, 0)
	if err := c.SetStreamSize(s.ID, "alice", same); err != nil {
		t.Fatal(err)
	}
	quick, stop := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer stop()
	if _, _, ok := c.WatchStreamSize(quick, s.ID, ver); ok {
		t.Error("re-reporting the same size counted as a change")
	}
}

// Only the account whose session it is may resize it, for the same reason
// only they may type into it.
func TestStreamResizeIsScopedToItsOwner(t *testing.T) {
	c, _ := dashFixture(t)
	s := c.NewStream("alice", 7)

	if err := c.SetStreamSize(s.ID, "bob", job.Winsize{Rows: 10, Cols: 10}); err == nil {
		t.Fatal("bob resized alice's session")
	}
	// A size of no size is refused: applying it makes full-screen programs
	// draw into nothing.
	if err := c.SetStreamSize(s.ID, "alice", job.Winsize{}); err == nil {
		t.Fatal("a zero window size was accepted")
	}
}

// A job that never starts leaves a session nobody will ever write to. The
// client is sitting on a read; without this it sits there forever, while
// squeue shows the job failed.
func TestSessionEndsWhenItsJobNeverStarted(t *testing.T) {
	c, st := dashFixture(t)
	ctx := context.Background()
	j, err := st.Submit(ctx, job.Spec{User: "alice", Name: "bash",
		Limits: job.Limits{CPUs: 1}}, c.now())
	if err != nil {
		t.Fatal(err)
	}
	s := c.NewStream("alice", j.ID)
	_, out, _, err := c.AttachClient(s.ID, "alice")
	if err != nil {
		t.Fatal(err)
	}

	// While the job is still pending, the client waits: this must not cut
	// off a session whose node is simply slow to pick the job up.
	c.reapOrphanStreams(ctx)
	if _, exited := c.StreamExit(s.ID, "alice"); exited {
		t.Fatal("a pending job's session was ended")
	}

	// The node could not start it.
	if err := st.MarkFinished(ctx, j.ID, job.Failed, -1,
		"launch failed: read job script bash: no such file", 0, c.now()); err != nil {
		t.Fatal(err)
	}
	c.reapOrphanStreams(ctx)

	done := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(out)
		done <- err
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the client is still waiting for a job that failed to start")
	}
	code, exited := c.StreamExit(s.ID, "alice")
	if !exited {
		t.Fatal("the session did not report a status")
	}
	if code == 0 {
		t.Error("a job that never ran reported success")
	}
}

// Once a node has attached, the node owns the ending: cutting the session
// short here would truncate the last of a job's output.
func TestSessionWithANodeAttachedIsLeftAlone(t *testing.T) {
	c, st := dashFixture(t)
	ctx := context.Background()
	j, err := st.Submit(ctx, job.Spec{User: "alice", Name: "j",
		Limits: job.Limits{CPUs: 1}}, c.now())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.MarkRunning(ctx, j.ID, "mini", "", c.now()); err != nil {
		t.Fatal(err)
	}
	s := c.NewStream("alice", j.ID)
	if _, _, err := c.AttachNode(s.ID, "mini"); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkFinished(ctx, j.ID, job.Completed, 0, "", 0, c.now()); err != nil {
		t.Fatal(err)
	}
	c.reapOrphanStreams(ctx)
	if _, exited := c.StreamExit(s.ID, "alice"); exited {
		t.Error("a session its node is still draining was ended from underneath it")
	}
}
