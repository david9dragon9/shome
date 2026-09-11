//go:build darwin

package daemon

import (
	"os"

	"github.com/davidwu/shome/internal/platform"
	"github.com/davidwu/shome/internal/platform/darwin"
)

// newBackend builds this machine's node backend.
//
// stateRoot is the whole installation's directory. It differs from root only
// for the agent a controller runs for itself, whose scratch lives one level
// down -- and disk accounting needs the outer one, or a controller reports a
// fraction of what it is actually using.
func newBackend(root string) (platform.Backend, error) {
	return newBackendAt(root, root)
}

func newBackendAt(root, stateRoot string) (platform.Backend, error) {
	home, _ := os.UserHomeDir()
	b := darwin.New(root, home)
	b.StateRoot = stateRoot
	return b, nil
}
