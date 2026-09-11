//go:build !darwin && !linux

package main

import (
	"fmt"
	"runtime"

	"github.com/davidwu/shome/internal/platform"
)

func newLocalBackend(root string) (platform.Backend, error) {
	return nil, fmt.Errorf("no shome node backend for %s", runtime.GOOS)
}
