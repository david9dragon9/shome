package loginnode

import (
	"errors"
	"syscall"
)

// killGroup signals a whole process group.
//
// The shell is a session leader with its own group, so one signal reaches it
// and everything it started. Killing only the shell would leave its children
// -- a python process, a uv download -- running with a terminal nobody reads.
func killGroup(pid int) {
	// Negative pid means the group. SIGHUP first, which is what a terminal
	// closing means and what shells and their children are written to
	// handle; then SIGKILL for anything that ignored it.
	syscall.Kill(-pid, syscall.SIGHUP)
	syscall.Kill(-pid, syscall.SIGKILL)
}

func errorsAs(err error, target any) bool { return errors.As(err, target) }

// groupAttr puts a child in its own process group.
//
// So that a command whose client disconnected can be signalled as a tree. A
// pty session gets this from Setsid instead, which also makes the terminal
// its controlling one.
func groupAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }
