package main

import (
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/davidwu/shome/internal/ctl"
	"github.com/davidwu/shome/internal/tui"
)

// `shome top` is the cluster's live dashboard: what every machine is doing,
// what is queued, and the controls to act on both.
//
// Modelled on nvitop and htop, but for a set of machines rather than one. The
// design constraint that shapes it: this is a cluster of computers people are
// also *using*, so the display has to answer "is the cluster busy?" and "is
// anyone's machine being ruined?" at the same glance. That is why owner
// signals -- battery, thermals, keyboard idle -- sit next to utilisation
// rather than being buried in a detail view.

// refresh cadence. Faster than this samples nothing new: the numbers arrive on
// node heartbeats, so polling quicker only redraws the same values.
const (
	defaultInterval = 2 * time.Second
	minInterval     = 500 * time.Millisecond
	maxInterval     = 30 * time.Second
)

type pane int

const (
	paneNodes pane = iota
	paneJobs
)

// topUI is all the dashboard's mutable state.
type topUI struct {
	mu       sync.Mutex
	dash     *ctl.Dashboard
	err      error
	interval time.Duration
	paused   bool

	focus    pane
	nodeSel  int
	jobSel   int
	filter   string
	filterOn bool

	// message is transient feedback from an action, shown in the footer.
	message   string
	messageAt time.Time
	// confirm holds a pending destructive action awaiting y/n.
	confirm     string
	confirmFunc func() error

	showHelp bool
	quit     bool
}

func top(args []string) error {
	ui := &topUI{interval: defaultInterval, focus: paneJobs}
	once := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-i", "--interval":
			if i+1 >= len(args) {
				return fmt.Errorf("-i needs a duration, e.g. -i 1s")
			}
			i++
			d, err := time.ParseDuration(args[i])
			if err != nil {
				return fmt.Errorf("invalid interval %q", args[i])
			}
			ui.interval = clampInterval(d)
		case "-1", "--once":
			once = true
		case "--no-color":
			tui.NoColor = true
		default:
			return fmt.Errorf("unknown option %q for 'shome top'", args[i])
		}
	}

	// A dashboard piped into a file should print one frame and stop, rather
	// than filling it with escape sequences forever.
	if once || !tui.IsTerminal() {
		tui.NoColor = tui.NoColor || !tui.IsTerminal()
		if err := ui.fetch(); err != nil {
			return err
		}
		w, h := tui.Size()
		// Piping to a file wants the whole frame, not one screenful; an
		// explicit LINES, or a real terminal, means the caller has a height
		// in mind and it should be respected.
		if !tui.IsTerminal() && os.Getenv("LINES") == "" {
			h = 1000
		}
		fmt.Print(ui.render(w, h, false))
		return nil
	}
	return ui.run()
}

func clampInterval(d time.Duration) time.Duration {
	if d < minInterval {
		return minInterval
	}
	if d > maxInterval {
		return maxInterval
	}
	return d
}

func (u *topUI) run() error {
	term, err := tui.MakeRaw()
	if err != nil {
		return fmt.Errorf("this terminal does not support interactive mode: %w\n"+
			"try:  shome top -1", err)
	}
	// Restore on every exit path. A dashboard that leaves the terminal in raw
	// mode with no cursor is a worse bug than anything it could display.
	restore := func() {
		term.Restore()
		fmt.Print(tui.CursorShow + tui.AltScreenOff)
	}
	defer restore()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)

	fmt.Print(tui.AltScreenOn + tui.CursorHide)

	keys := make(chan byte, 16)
	go readKeys(keys)

	u.fetchInto()
	u.draw()

	tick := time.NewTicker(u.interval)
	defer tick.Stop()
	redraw := make(chan struct{}, 1)

	for {
		select {
		case <-sig:
			return nil
		case <-tick.C:
			if !u.paused {
				u.fetchInto()
			}
			u.draw()
		case b, ok := <-keys:
			if !ok {
				// stdin closed: there is no way left to drive this, so exit
				// rather than redraw forever. Without it, a dashboard started
				// with its input redirected never returns.
				return nil
			}
			u.handleKey(b)
			u.mu.Lock()
			q, iv := u.quit, u.interval
			u.mu.Unlock()
			if q {
				return nil
			}
			tick.Reset(iv)
			u.draw()
		case <-redraw:
			u.draw()
		}
	}
}

// readKeys forwards raw keystrokes. Escape sequences for the arrow keys arrive
// as three bytes; they are translated to the equivalent letter so the event
// handler has one representation to deal with.
func readKeys(out chan<- byte) {
	defer close(out)
	buf := make([]byte, 16)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			return
		}
		// Defensive: a zero-byte read is not end of input. With VMIN 1 this
		// should not happen, but treating it as EOF would silently close the
		// dashboard, and that failure looks like a crash rather than a bug.
		if n == 0 {
			continue
		}
		for i := 0; i < n; i++ {
			if buf[i] == 0x1b && i+2 < n && buf[i+1] == '[' {
				switch buf[i+2] {
				case 'A':
					out <- 'k' // up
				case 'B':
					out <- 'j' // down
				case 'C':
					out <- 'l' // right
				case 'D':
					out <- 'h' // left
				}
				i += 2
				continue
			}
			out <- buf[i]
		}
	}
}

func (u *topUI) fetch() error {
	var d ctl.Dashboard
	if err := call("GET", "/dashboard", nil, &d); err != nil {
		return err
	}
	u.mu.Lock()
	u.dash, u.err = &d, nil
	u.mu.Unlock()
	return nil
}

func (u *topUI) fetchInto() {
	if err := u.fetch(); err != nil {
		u.mu.Lock()
		u.err = err
		u.mu.Unlock()
	}
}

func (u *topUI) draw() {
	w, h := tui.Size()
	u.mu.Lock()
	out := u.render(w, h, true)
	u.mu.Unlock()
	os.Stdout.WriteString(tui.Home + out + tui.ClearBelow)
}

func (u *topUI) note(format string, a ...any) {
	u.message = fmt.Sprintf(format, a...)
	u.messageAt = time.Now()
}
