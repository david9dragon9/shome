// Command shomectld runs the shome controller.
//
// It is a thin wrapper: the implementation lives in internal/daemon so that
// the single `shome` binary can be a controller, an agent, or a client. Prefer
// `shome up`, which picks sane defaults, runs in the background, manages its
// own pid file and log, and can be stopped with `shome down`.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/davidwu/shome/internal/ctl"
	"github.com/davidwu/shome/internal/daemon"
)

func main() {
	var (
		root       = flag.String("root", ctl.DefaultRoot(), "state directory")
		listen     = flag.String("listen", "0.0.0.0:7817", "address for node agents")
		node       = flag.String("node", daemon.Hostname(), "name for this machine's local agent")
		localAgent = flag.Bool("local-agent", true, "also run an agent for this machine")
		advertise  = flag.String("advertise", "", "extra name(s) or IP(s) agents will dial, comma-separated")
		webAddr    = flag.String("web", "", "serve the admin console here, e.g. 127.0.0.1:7820")
		bootstrap  = flag.Bool("bootstrap", true, "serve the one-command join service on the next port up")
		verbose    = flag.Bool("v", false, "verbose logging")
	)
	flag.Parse()

	lvl := slog.LevelInfo
	if *verbose {
		lvl = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	daemon.WritePID(*root)
	defer daemon.ClearPID(*root)

	if err := daemon.RunController(ctx, daemon.ControllerConfig{
		Root: *root, Listen: *listen, Node: *node, LocalAgent: *localAgent,
		Advertise: *advertise, WebAddr: *webAddr, Bootstrap: *bootstrap, Log: log,
	}); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}
