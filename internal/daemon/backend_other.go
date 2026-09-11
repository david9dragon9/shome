//go:build !darwin && !linux

package daemon

import (
	"fmt"
	"runtime"

	"github.com/davidwu/shome/internal/platform"
)

// A node backend is how shome isolates and limits jobs. Without one there is
// no isolation at all, so refusing to start beats running work unprotected.
func newBackend(root string) (platform.Backend, error) {
	return newBackendAt(root, root)
}

func newBackendAt(root, stateRoot string) (platform.Backend, error) {
	return nil, fmt.Errorf("shome has no node backend for %s yet (macOS and Linux are supported)", runtime.GOOS)
}
