//go:build !unix

package daemon

import "syscall"

func detachAttr() *syscall.SysProcAttr { return nil }
