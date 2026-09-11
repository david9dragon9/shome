//go:build !darwin && !linux

package main

import (
	"fmt"
	"runtime"

	"github.com/davidwu/shome/internal/platform"
)

// The Linux backend (real users, cgroups v2, namespaces, Landlock) is M6.
// Failing loudly beats silently running jobs with no isolation at all.
func newBackend(root string) (platform.Backend, error) {
	return nil, fmt.Errorf("no shome node backend for %s yet; macOS only in M1", runtime.GOOS)
}
