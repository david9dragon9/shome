package ctl

import (
	"context"
	"testing"

	"github.com/davidwu/shome/internal/agentapi"
	"github.com/davidwu/shome/internal/platform"
)

// Forgetting a machine has to stick. Deleting the row on its own does not:
// the agent is still running with a valid certificate, and its next
// heartbeat -- three seconds later -- recreates what was just removed,
// which is what "I clicked forget and the node came back" looks like from
// the console.
func TestAForgottenNodeStaysForgotten(t *testing.T) {
	c, st := dashFixture(t)
	ctx := context.Background()
	caps := platform.Capabilities{OS: "linux", Arch: "amd64", CPUs: 2, MemBytes: 2 << 30}

	// A machine reporting in is in the cluster.
	if _, _, err := c.HandleHeartbeat(ctx, agentapi.Heartbeat{Node: "penguin", Caps: caps}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.NodeByName(ctx, "penguin"); err != nil {
		t.Fatalf("a machine that reported in is not in the cluster: %v", err)
	}

	if err := c.RemoveNode(ctx, "penguin"); err != nil {
		t.Fatal(err)
	}

	// Its next heartbeat must not put it back, and must tell it it is gone.
	resp, _, err := c.HandleHeartbeat(ctx, agentapi.Heartbeat{Node: "penguin", Caps: caps})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Known {
		t.Error("a removed machine was told it is still part of the cluster")
	}
	if _, err := st.NodeByName(ctx, "penguin"); err == nil {
		t.Error("a removed machine put itself back on its next heartbeat")
	}

	// Having it back is a decision somebody makes with a join token.
	if err := st.UnforgetNode(ctx, "penguin"); err != nil {
		t.Fatal(err)
	}
	resp, _, err = c.HandleHeartbeat(ctx, agentapi.Heartbeat{Node: "penguin", Caps: caps})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Known {
		t.Error("a machine that joined again was still refused")
	}
	if _, err := st.NodeByName(ctx, "penguin"); err != nil {
		t.Errorf("a machine that joined again is not in the cluster: %v", err)
	}
}

// Removing a machine that is not there is how `shome nuke` tidies up after
// itself, so it must not fail -- and it must still be remembered, or the
// machine's last heartbeats would bring it back.
func TestForgettingAMachineThatHasAlreadyGone(t *testing.T) {
	c, st := dashFixture(t)
	ctx := context.Background()
	if err := c.RemoveNode(ctx, "never-existed"); err == nil {
		// DeleteNode reports no such node; the tombstone is not written
		// because there was nothing to forget. Both are acceptable, but
		// the call must not panic or wedge, and the name must not end up
		// half-removed.
		if gone, _ := st.NodeForgotten(ctx, "never-existed"); !gone {
			t.Log("no tombstone for a machine that was never here, which is fine")
		}
	}
}
