//go:build darwin

package main

import (
	"os"

	"github.com/davidwu/shome/internal/platform"
	"github.com/davidwu/shome/internal/platform/darwin"
)

// newLocalBackend builds this machine's backend, so owner commands can read
// its own metrics without a controller.
func newLocalBackend(root string) (platform.Backend, error) {
	home, _ := os.UserHomeDir()
	return darwin.New(root, home), nil
}
