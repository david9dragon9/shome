//go:build unix

package daemon

import "syscall"

// detachAttr puts the daemon in its own session, so closing the terminal that
// started it does not send it a hangup.
func detachAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }
