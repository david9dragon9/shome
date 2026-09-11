//go:build linux

package main

import (
	"os"

	"github.com/davidwu/shome/internal/platform"
	"github.com/davidwu/shome/internal/platform/linux"
)

func newBackend(root string) (platform.Backend, error) {
	return linux.New(root, os.Getenv("HOME")), nil
}
