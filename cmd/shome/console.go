package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/davidwu/shome/internal/tui"
)

// console is the non-interactive form of the dashboard: the same frame `shome
// top` draws, reprinted on a timer with no raw mode and no key handling.
//
// It exists for the cases where taking over the terminal is wrong -- a wall
// display, a tmux pane, piping to a file, a machine where stdin is not a
// terminal at all. Sharing the renderer with `top` rather than keeping a
// second implementation means the two cannot drift into disagreeing about the
// state of the cluster, which was the previous arrangement.
func console(args []string) error {
	interval := 2 * time.Second
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-i", "--interval":
			if i+1 >= len(args) {
				return fmt.Errorf("-i needs a duration, e.g. -i 5s")
			}
			i++
			d, err := time.ParseDuration(args[i])
			if err != nil {
				return fmt.Errorf("invalid interval %q", args[i])
			}
			interval = clampInterval(d)
		case "--no-color":
			tui.NoColor = true
		default:
			return fmt.Errorf("unknown option %q for 'shome console'", args[i])
		}
	}
	if !tui.IsTerminal() {
		tui.NoColor = true
	}

	ui := &topUI{interval: interval}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)
	defer fmt.Print(tui.CursorShow)
	fmt.Print(tui.CursorHide)

	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		ui.fetchInto()
		w, h := tui.Size()
		// Home-and-clear-below rather than a full clear: a full clear makes
		// the screen flicker on every refresh.
		os.Stdout.WriteString(tui.Home + tui.ClearBelow + ui.render(w, h, false))
		select {
		case <-sig:
			fmt.Println()
			return nil
		case <-tick.C:
		}
	}
}
