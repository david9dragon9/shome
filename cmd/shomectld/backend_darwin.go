//go:build darwin

package main

import (
	"os"

	"github.com/davidwu/shome/internal/platform"
	"github.com/davidwu/shome/internal/platform/darwin"
)

func newBackend(root string) (platform.Backend, error) {
	home, _ := os.UserHomeDir()
	return darwin.New(root, home), nil
}
