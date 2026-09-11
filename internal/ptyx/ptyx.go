// Package ptyx allocates pseudo-terminals for interactive sessions.
//
// Written against golang.org/x/sys rather than pulling in a pty library: the
// whole of it is three ioctls per platform, and shome's dependency list is
// short on purpose -- every module in it is one more thing an owner has to
// trust to run on their computer.
//
// A pseudo-terminal is what makes an interactive session behave like a
// terminal instead of a pipe. Without one, a shell does not print a prompt,
// line editing does not work, programs that check isatty take their
// non-interactive path, and Ctrl-C reaches nothing.
package ptyx

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

// PTY is an allocated pseudo-terminal pair.
type PTY struct {
	// Master is the side shome reads and writes: bytes written here appear
	// as keystrokes to the program, and its output is read back.
	Master *os.File
	// Slave is the side the child process uses as its terminal. Closed in
	// the parent once the child has been started.
	Slave *os.File
}

// Open allocates a pseudo-terminal.
func Open() (*PTY, error) {
	m, name, err := openMaster()
	if err != nil {
		return nil, err
	}
	s, err := os.OpenFile(name, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		m.Close()
		return nil, fmt.Errorf("open %s: %w", name, err)
	}
	return &PTY{Master: m, Slave: s}, nil
}

// Close releases both ends.
//
// Idempotent: a session that fails partway through setup closes explicitly
// and again from a defer, and the second call reporting "file already
// closed" would turn ordinary cleanup into a logged error.
func (p *PTY) Close() error {
	var first error
	if p.Slave != nil {
		if err := p.Slave.Close(); err != nil && first == nil {
			first = err
		}
		p.Slave = nil
	}
	if p.Master != nil {
		if err := p.Master.Close(); err != nil && first == nil {
			first = err
		}
		p.Master = nil
	}
	return first
}

// Resize sets the terminal's window size.
//
// Applied to the master, which the kernel propagates to the slave along with
// a SIGWINCH to the foreground process group -- which is how a full-screen
// program learns to redraw when the user's window changes.
func (p *PTY) Resize(rows, cols, xpixel, ypixel uint16) error {
	return unix.IoctlSetWinsize(int(p.Master.Fd()), unix.TIOCSWINSZ, &unix.Winsize{
		Row: rows, Col: cols, Xpixel: xpixel, Ypixel: ypixel,
	})
}

// Attach wires a command up to the terminal as its controlling terminal.
//
// Setsid plus Setctty is what makes the child a session leader with this
// terminal in charge, so job control works and Ctrl-C becomes a SIGINT to the
// child rather than to shome. Without it the signal would arrive here and
// take the daemon down with it.
func (p *PTY) Attach(cmd *exec.Cmd) {
	cmd.Stdin, cmd.Stdout, cmd.Stderr = p.Slave, p.Slave, p.Slave
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
	cmd.SysProcAttr.Setctty = true
}

// CloseSlave releases the parent's copy of the child's terminal.
//
// Must be called after starting the child and before reading the master, or
// reads never see end-of-file: the kernel reports the terminal as closed only
// when the last descriptor for it is gone, and the parent holding one open
// keeps the session looking alive forever after the shell has exited.
func (p *PTY) CloseSlave() {
	if p.Slave != nil {
		p.Slave.Close()
		p.Slave = nil
	}
}
