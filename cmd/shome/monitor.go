package main

import (
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/davidwu/shome/internal/agent"
	"github.com/davidwu/shome/internal/ctl"
	"github.com/davidwu/shome/internal/owner"
	"github.com/davidwu/shome/internal/platform"
	"github.com/davidwu/shome/internal/tui"
)

// `shome monitor` is the machine owner's view: what the cluster is doing to
// *this* computer.
//
// Separate from `shome top`, which is the admin's view of the whole cluster,
// because the two answer different questions and the owner's has to work in
// conditions the admin's does not. It reads the status file the agent
// publishes locally, so it needs no controller, no token, and no network --
// and "what is shome doing to my machine" is a question that matters most
// when something is wrong, which is exactly when the cluster may be
// unreachable.
//
// The actions it offers are the ones an owner has unilaterally: pause and
// resume. Neither needs the cluster's agreement, and both take effect at once.

type monitorUI struct {
	mu       sync.Mutex
	st       agent.LocalStatus
	err      error
	root     string
	interval time.Duration
	paused   bool // display paused, not the node

	message     string
	messageAt   time.Time
	confirm     string
	confirmFunc func() error

	showHelp bool
	quit     bool
}

func monitor(args []string) error {
	ui := &monitorUI{interval: 2 * time.Second, root: ctl.DefaultRoot()}
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
			return fmt.Errorf("unknown option %q for 'shome monitor'", args[i])
		}
	}

	if once || !tui.IsTerminal() {
		tui.NoColor = tui.NoColor || !tui.IsTerminal()
		ui.fetch()
		w, h := tui.Size()
		if !tui.IsTerminal() && os.Getenv("LINES") == "" {
			h = 1000
		}
		fmt.Print(ui.render(w, h, false))
		return nil
	}
	return ui.run()
}

func (u *monitorUI) run() error {
	term, err := tui.MakeRaw()
	if err != nil {
		return fmt.Errorf("this terminal does not support interactive mode: %w\n"+
			"try:  shome monitor -1", err)
	}
	defer func() {
		term.Restore()
		fmt.Print(tui.CursorShow + tui.AltScreenOff)
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)
	fmt.Print(tui.AltScreenOn + tui.CursorHide)

	keys := make(chan byte, 16)
	go readKeys(keys)

	u.fetch()
	u.draw()
	tick := time.NewTicker(u.interval)
	defer tick.Stop()

	for {
		select {
		case <-sig:
			return nil
		case <-tick.C:
			if !u.paused {
				u.fetch()
			}
			u.draw()
		case b, ok := <-keys:
			if !ok {
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
		}
	}
}

// fetch reads the agent's published status.
//
// A stale file is reported as stale rather than shown as current: an agent
// that has stopped writing is exactly the situation where believing the last
// numbers would be worst.
func (u *monitorUI) fetch() {
	st, err := agent.ReadStatus(u.root)
	u.mu.Lock()
	defer u.mu.Unlock()
	if err != nil {
		u.err = err
		return
	}
	u.st, u.err = st, nil
}

func (u *monitorUI) draw() {
	w, h := tui.Size()
	u.mu.Lock()
	out := u.render(w, h, true)
	u.mu.Unlock()
	os.Stdout.WriteString(tui.Home + out + tui.ClearBelow)
}

func (u *monitorUI) note(format string, a ...any) {
	u.message = fmt.Sprintf(format, a...)
	u.messageAt = time.Now()
}

// stale reports whether the agent has stopped publishing.
func (u *monitorUI) stale() bool {
	return u.st.At.IsZero() || time.Since(u.st.At) > 30*time.Second
}

func (u *monitorUI) handleKey(b byte) {
	u.mu.Lock()
	defer u.mu.Unlock()

	if u.confirm != "" {
		if b == 'y' || b == 'Y' {
			fn := u.confirmFunc
			u.confirm, u.confirmFunc = "", nil
			if fn != nil {
				u.mu.Unlock()
				err := fn()
				u.mu.Lock()
				if err != nil {
					u.note("failed: %s", err)
				} else {
					u.note("done")
					u.mu.Unlock()
					u.fetch()
					u.mu.Lock()
				}
			}
			return
		}
		u.confirm, u.confirmFunc = "", nil
		u.note("cancelled")
		return
	}

	switch b {
	case 'q', 3:
		u.quit = true
	case '?':
		u.showHelp = !u.showHelp
	case 'p':
		// The owner's veto. A local file with no network call, so it works
		// when the controller does not and survives a reboot.
		u.confirm = "Stop contributing this machine now? Running jobs are suspended."
		u.confirmFunc = func() error {
			m := owner.NewManager(u.root, owner.DarwinSensor{})
			return m.Pause("paused from the monitor")
		}
	case 'r':
		u.mu.Unlock()
		m := owner.NewManager(u.root, owner.DarwinSensor{})
		err := m.Resume()
		u.mu.Lock()
		if err != nil {
			u.note("failed: %s", err)
		} else {
			u.note("contributing again")
			u.mu.Unlock()
			u.fetch()
			u.mu.Lock()
		}
	case 'f':
		u.mu.Unlock()
		u.fetch()
		u.mu.Lock()
	case ' ':
		u.paused = !u.paused
		if u.paused {
			u.note("display frozen; space to resume")
		} else {
			u.note("live again")
		}
	case '+', '=':
		u.interval = clampInterval(u.interval - 500_000_000)
		u.note("refreshing every %s", u.interval)
	case '-', '_':
		u.interval = clampInterval(u.interval + 500_000_000)
		u.note("refreshing every %s", u.interval)
	}
}

var _ = sort.Strings
var _ = strings.TrimSpace
var _ = platform.Unknown
