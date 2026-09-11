package ctl

import (
	"testing"

	"github.com/davidwu/shome/internal/job"
)

// A shell at a prompt gets network, like a login session does. Somebody who
// runs `srun --pty bash` and then `uv add torch` should not be told "dns
// error: nodename nor servname provided" -- a message that names neither
// shome nor the flag that would have fixed it.
func TestAShellGetsNetwork(t *testing.T) {
	spec := job.Spec{PTY: true, Interactive: true}
	NormalizeSpec(&spec)
	if !spec.Limits.Network {
		t.Error("an interactive shell has no network, so no package manager works in it")
	}
}

// Batch keeps the deny-by-default: nobody is watching, and a script that
// reaches the network should say so.
func TestABatchJobDoesNotGetNetworkByAccident(t *testing.T) {
	for _, spec := range []job.Spec{
		{},
		{Interactive: true}, // srun without a terminal: watch output, not a shell
	} {
		got := spec
		NormalizeSpec(&got)
		if got.Limits.Network {
			t.Errorf("%+v was given network it did not ask for", spec)
		}
	}
	// And an explicit request still stands.
	asked := job.Spec{Limits: job.Limits{Network: true}}
	NormalizeSpec(&asked)
	if !asked.Limits.Network {
		t.Error("--network was dropped")
	}
}
