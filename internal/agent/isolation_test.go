package agent

import (
	"strings"
	"testing"

	"github.com/davidwu/shome/internal/platform"
)

// faultBackend is a backend that only answers the isolation question.
//
// The interface is embedded rather than implemented: every other method is
// nil and would panic if called, which is the point -- if this test starts
// exercising something else, it should fail loudly rather than quietly
// testing a stub.
type faultBackend struct {
	platform.Backend
	fault string
}

func (f faultBackend) IsolationFault() string { return f.fault }

// A machine that cannot confine a job must not accept cluster work.
//
// Both backends had a path where a missing sandbox meant the job ran anyway,
// with no confinement and no error: on Linux the bubblewrap wrapper was
// skipped when bubblewrap was absent, and on macOS the profile was skipped
// when sandbox-exec was not found. The tier report described it in text that
// nothing acted on.
func TestIsolationFaultDrainsTheNode(t *testing.T) {
	const why = "bubblewrap not installed: this machine cannot confine a job"

	a := &Agent{Backend: faultBackend{fault: why}}
	action, reason := a.IsolationDrain()
	if action != "drain" {
		t.Errorf("action = %q, want drain -- the node would accept work it "+
			"cannot confine", action)
	}
	if !strings.Contains(reason, "bubblewrap") {
		t.Errorf("reason = %q, does not say what is missing", reason)
	}

	// A machine that can confine is left alone.
	a = &Agent{Backend: faultBackend{}}
	if action, _ := a.IsolationDrain(); action != "" {
		t.Errorf("a machine that can isolate was drained: %q", action)
	}

	// No backend at all is not this rule's business to judge.
	a = &Agent{}
	if action, _ := a.IsolationDrain(); action != "" {
		t.Errorf("action = %q with no backend", action)
	}
}
