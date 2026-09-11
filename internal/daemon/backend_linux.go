//go:build linux

package daemon

import (
	"os"

	"github.com/davidwu/shome/internal/platform"
	"github.com/davidwu/shome/internal/platform/linux"
)

// newBackend builds this machine's node backend. See the darwin version for
// why stateRoot is separate.
func newBackend(root string) (platform.Backend, error) {
	return newBackendAt(root, root)
}

func newBackendAt(root, stateRoot string) (platform.Backend, error) {
	b := linux.New(root, os.Getenv("HOME"))
	b.StateRoot = stateRoot
	return b, nil
}
