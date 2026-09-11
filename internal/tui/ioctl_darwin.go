//go:build darwin

package tui

import "golang.org/x/sys/unix"

// The termios ioctls have different names per platform; everything else in
// this package is portable across them.
const (
	ioctlGet = unix.TIOCGETA
	ioctlSet = unix.TIOCSETA
)
