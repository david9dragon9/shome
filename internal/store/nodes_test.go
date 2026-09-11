package store

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/davidwu/shome/internal/job"
	"github.com/davidwu/shome/internal/platform"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestJoinTokenIsSingleUse(t *testing.T) {
	s := newStore(t)
	ctx, now := context.Background(), time.Now()
	tok, err := s.CreateJoinToken(ctx, time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RedeemJoinToken(ctx, tok, "node-a", now); err != nil {
		t.Fatalf("first redemption should succeed: %v", err)
	}
	err = s.RedeemJoinToken(ctx, tok, "node-b", now)
	if err == nil {
		t.Fatal("token was reusable; a leaked token would let anyone join")
	}
	if !contains(err.Error(), "already used") {
		t.Errorf("unhelpful error: %v", err)
	}
}

func TestJoinTokenExpires(t *testing.T) {
	s := newStore(t)
	ctx, now := context.Background(), time.Now()
	tok, _ := s.CreateJoinToken(ctx, time.Minute, now)
	err := s.RedeemJoinToken(ctx, tok, "n", now.Add(2*time.Minute))
	if err == nil {
		t.Fatal("expired token was accepted")
	}
	if !contains(err.Error(), "expired") {
		t.Errorf("unhelpful error: %v", err)
	}
}

func TestUnknownTokenRejected(t *testing.T) {
	s := newStore(t)
	if err := s.RedeemJoinToken(context.Background(), "not-a-real-token", "n", time.Now()); err == nil {
		t.Fatal("an unknown token was accepted")
	}
}

// Two agents racing with the same token must not both join.
func TestJoinTokenRaceOnlyOneWinner(t *testing.T) {
	s := newStore(t)
	ctx, now := context.Background(), time.Now()
	tok, _ := s.CreateJoinToken(ctx, time.Hour, now)

	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.RedeemJoinToken(ctx, tok, "racer", now); err == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Errorf("%d concurrent redemptions succeeded, want exactly 1", wins)
	}
}

func TestDrainSurvivesAgentRestart(t *testing.T) {
	s := newStore(t)
	ctx, now := context.Background(), time.Now()
	caps := platform.Capabilities{CPUs: 4}
	s.UpsertNode(ctx, "n1", "10.0.0.1", caps, now)
	if err := s.SetNodeState(ctx, "n1", NodeDrain, "owner paused"); err != nil {
		t.Fatal(err)
	}
	// Agent restarts and re-registers.
	s.UpsertNode(ctx, "n1", "10.0.0.1", caps, now.Add(time.Minute))

	ns, _ := s.Nodes(ctx)
	if len(ns) != 1 {
		t.Fatalf("expected 1 node, got %d", len(ns))
	}
	// The owner's pause must outlive a reboot, or it is not really a pause.
	if ns[0].State != NodeDrain {
		t.Errorf("state = %s after restart, want DRAIN", ns[0].State)
	}
}

func TestExpireNodesAndRequeue(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	t0 := time.Now()
	s.UpsertNode(ctx, "n1", "10.0.0.1", platform.Capabilities{CPUs: 4}, t0)

	// A node that has not been heard from is DOWN, not silently still UP.
	gone, err := s.ExpireNodes(ctx, 30*time.Second, t0.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(gone) != 1 || gone[0] != "n1" {
		t.Fatalf("expected n1 to expire, got %v", gone)
	}
	ns, _ := s.Nodes(ctx)
	if ns[0].State != NodeDown {
		t.Errorf("state = %s, want DOWN", ns[0].State)
	}

	// A heartbeat brings it back.
	s.Heartbeat(ctx, "n1", t0.Add(2*time.Minute))
	ns, _ = s.Nodes(ctx)
	if ns[0].State != NodeUp {
		t.Errorf("state = %s after heartbeat, want UP", ns[0].State)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

// A node that recovers must not keep advertising why it was down.
func TestRecoveredNodeClearsItsReason(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	now := time.Now()
	caps := platform.Capabilities{OS: "linux", CPUs: 4}

	if err := s.UpsertNode(ctx, "n1", "10.0.0.1", caps, now); err != nil {
		t.Fatal(err)
	}
	// It misses heartbeats and is marked down with a reason.
	if _, err := s.ExpireNodes(ctx, time.Nanosecond, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	n, _ := s.NodeByName(ctx, "n1")
	if n.State != NodeDown || n.Reason == "" {
		t.Fatalf("expected DOWN with a reason, got %s %q", n.State, n.Reason)
	}

	// It comes back.
	if err := s.UpsertNode(ctx, "n1", "10.0.0.1", caps, time.Now()); err != nil {
		t.Fatal(err)
	}
	n, _ = s.NodeByName(ctx, "n1")
	if n.State != NodeUp {
		t.Errorf("state = %s, want UP", n.State)
	}
	if n.Reason != "" {
		t.Errorf("reason = %q; a recovered node should not still explain why it was down", n.Reason)
	}
}

// A drain reason, by contrast, is still true and must survive.
func TestDrainReasonSurvivesHeartbeats(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	caps := platform.Capabilities{OS: "linux", CPUs: 4}
	s.UpsertNode(ctx, "n1", "10.0.0.1", caps, time.Now())
	s.SetNodeState(ctx, "n1", NodeDrain, "owner: stepping away")

	s.UpsertNode(ctx, "n1", "10.0.0.1", caps, time.Now())
	n, _ := s.NodeByName(ctx, "n1")
	if n.State != NodeDrain {
		t.Errorf("state = %s; a heartbeat must not undo a drain", n.State)
	}
	if n.Reason != "owner: stepping away" {
		t.Errorf("reason = %q; the owner's reason must survive", n.Reason)
	}
}

func TestDeleteNodeRefusesWhileJobsRun(t *testing.T) {
	s := newStore(t)
	ctx, now := context.Background(), time.Now()
	if err := s.UpsertNode(ctx, "mini", "10.0.0.1", platform.Capabilities{CPUs: 4}, now); err != nil {
		t.Fatal(err)
	}
	j, err := s.Submit(ctx, job.Spec{Name: "j", User: "u", Script: "/bin/true", ArrayTaskID: -1}, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkRunning(ctx, j.ID, "mini", "", now); err != nil {
		t.Fatal(err)
	}
	// Forgetting a node mid-job would orphan the record of where work ran.
	if err := s.DeleteNode(ctx, "mini"); err == nil {
		t.Fatal("DeleteNode succeeded while a job was running on the node")
	}
	if n, err := s.NodeByName(ctx, "mini"); err != nil || n == nil {
		t.Fatal("node was removed despite the error")
	}
}

func TestDeleteNodeKeepsJobHistory(t *testing.T) {
	s := newStore(t)
	ctx, now := context.Background(), time.Now()
	if err := s.UpsertNode(ctx, "mini", "10.0.0.1", platform.Capabilities{CPUs: 4}, now); err != nil {
		t.Fatal(err)
	}
	j, err := s.Submit(ctx, job.Spec{Name: "j", User: "u", Script: "/bin/true", ArrayTaskID: -1}, now)
	if err != nil {
		t.Fatal(err)
	}
	s.MarkRunning(ctx, j.ID, "mini", "", now)
	s.MarkFinished(ctx, j.ID, job.Completed, 0, "", 0, now)

	if err := s.DeleteNode(ctx, "mini"); err != nil {
		t.Fatalf("DeleteNode: %v", err)
	}
	if n, _ := s.NodeByName(ctx, "mini"); n != nil {
		t.Error("node still registered after DeleteNode")
	}
	// Accounting must still say where the work ran.
	got, err := s.Get(ctx, j.ID)
	if err != nil || got == nil {
		t.Fatal("job history was destroyed with the node")
	}
	if got.Node != "mini" {
		t.Errorf("job node = %q, want it preserved as mini", got.Node)
	}
}

func TestDeleteUnknownNode(t *testing.T) {
	s := newStore(t)
	if err := s.DeleteNode(context.Background(), "nope"); err == nil {
		t.Error("DeleteNode on an unknown node should report it")
	}
}
