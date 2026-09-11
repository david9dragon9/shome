//go:build linux

package linux

import (
	"testing"

	"github.com/davidwu/shome/internal/job"
	"github.com/davidwu/shome/internal/platform"
)

// A machine with no bubblewrap must not run the command bare. It should never
// be offered work at all -- IsolationFault drains it -- but if it somehow is,
// failing to start beats running unconfined.
func TestNoBubblewrapDoesNotRunUnconfined(t *testing.T) {
	b := &Backend{Env: Environment{HasBwrap: false}}
	if got := b.IsolationFault(); got == "" {
		t.Error("a machine with no bubblewrap reported no isolation fault")
	}
	argv := b.sandboxArgv(&platform.Sandbox{ScratchDir: "/w"}, job.Spec{},
		[]string{"/bin/sh", "-c", "whoami"})
	if len(argv) > 0 && argv[0] == "/bin/sh" {
		t.Errorf("the command would run unconfined: %v", argv)
	}
}
