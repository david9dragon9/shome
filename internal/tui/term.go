package tui

import (
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

// Terminal handling: raw mode for single-keystroke input, and the window size.
//
// Raw mode is what makes the dashboard interactive rather than a repeating
// printout -- pressing 'k' should act, not wait for Enter. It also means this
// program must restore the terminal on every exit path, including a panic,
// or it leaves the user's shell with no echo and no line editing.

// Term is a terminal switched into raw mode.
type Term struct {
	fd      int
	restore *unix.Termios
}

// MakeRaw puts stdin into raw mode. Restore must be called before exit.
func MakeRaw() (*Term, error) {
	fd := int(os.Stdin.Fd())
	prev, err := unix.IoctlGetTermios(fd, ioctlGet)
	if err != nil {
		return nil, err
	}
	raw := *prev
	// Canonical mode and echo off: deliver each keystroke immediately and do
	// not print it. Signals stay enabled so Ctrl-C still works even if the
	// event loop wedges -- an unkillable full-screen program is a bad thing
	// to inflict on someone.
	raw.Lflag &^= unix.ECHO | unix.ICANON
	raw.Iflag &^= unix.ICRNL
	// VMIN 1 / VTIME 0: block until at least one byte arrives.
	//
	// The obvious alternative -- VMIN 0 with a VTIME timeout, so reads return
	// periodically -- does not work in Go. A timed-out read returns zero
	// bytes, and os.File.Read turns a zero-byte read into io.EOF, which is
	// indistinguishable from the terminal actually closing. A reader that
	// quits on EOF then exits about 100ms after starting, having drawn
	// exactly one frame.
	//
	// Blocking costs nothing here: reading happens on its own goroutine and
	// the redraw is driven by a ticker, not by input.
	raw.Cc[unix.VMIN] = 1
	raw.Cc[unix.VTIME] = 0
	if err := unix.IoctlSetTermios(fd, ioctlSet, &raw); err != nil {
		return nil, err
	}
	return &Term{fd: fd, restore: prev}, nil
}

// Restore puts the terminal back exactly as it was found.
func (t *Term) Restore() {
	if t == nil || t.restore == nil {
		return
	}
	unix.IoctlSetTermios(t.fd, ioctlSet, t.restore)
}

// Size returns the terminal's width and height in cells, with a usable
// fallback when it cannot be determined (a pipe, or a terminal that does not
// answer). Guessing 80x24 beats dividing by zero.
func Size() (width, height int) {
	// COLUMNS/LINES win when set. They are how a caller overrides the size for
	// a non-terminal destination, and how the layout gets tested at widths the
	// developer's own terminal does not happen to be.
	w := envInt("COLUMNS")
	h := envInt("LINES")
	if w > 0 && h > 0 {
		return w, h
	}
	ws, err := unix.IoctlGetWinsize(int(os.Stdout.Fd()), unix.TIOCGWINSZ)
	if err != nil || ws.Col == 0 || ws.Row == 0 {
		if w <= 0 {
			w = 80
		}
		if h <= 0 {
			h = 24
		}
		return w, h
	}
	if w <= 0 {
		w = int(ws.Col)
	}
	if h <= 0 {
		h = int(ws.Row)
	}
	return w, h
}

func envInt(name string) int {
	v, err := strconv.Atoi(os.Getenv(name))
	if err != nil || v <= 0 {
		return 0
	}
	return v
}

// IsTerminal reports whether stdout is a terminal, so the dashboard can refuse
// to take over something that is actually a pipe.
func IsTerminal() bool {
	_, err := unix.IoctlGetTermios(int(os.Stdout.Fd()), ioctlGet)
	return err == nil
}
