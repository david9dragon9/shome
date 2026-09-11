//go:build darwin

package ptyx

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// openMaster opens /dev/ptmx and returns the master together with the path of
// its slave.
//
// The macOS sequence differs from Linux: grant, then unlock, then ask the
// kernel for the name. Deriving the name arithmetically from a pty number, as
// one can on Linux, is not available here.
func openMaster() (*os.File, string, error) {
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		return nil, "", fmt.Errorf("open /dev/ptmx: %w", err)
	}
	fd := int(m.Fd())
	if err := unix.IoctlSetInt(fd, unix.TIOCPTYGRANT, 0); err != nil {
		m.Close()
		return nil, "", fmt.Errorf("grant pty: %w", err)
	}
	if err := unix.IoctlSetInt(fd, unix.TIOCPTYUNLK, 0); err != nil {
		m.Close()
		return nil, "", fmt.Errorf("unlock pty: %w", err)
	}
	// TIOCPTYGNAME writes a NUL-terminated path into a 128-byte buffer, the
	// size being fixed by the ioctl's encoding rather than by choice.
	var buf [128]byte
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd),
		uintptr(unix.TIOCPTYGNAME), uintptr(unsafe.Pointer(&buf[0]))); errno != 0 {
		m.Close()
		return nil, "", fmt.Errorf("get pty name: %w", errno)
	}
	n := 0
	for n < len(buf) && buf[n] != 0 {
		n++
	}
	return m, string(buf[:n]), nil
}
