// Command shomed runs a shome node agent.
//
// It is a thin wrapper: the implementation lives in internal/daemon so that
// the single `shome` binary can be a controller, an agent, or a client. Prefer
// `shome join`, which is what `shome invite` prints on the controller, and
// which needs neither a state directory nor a certificate name.
package main

import (
	"context"
	"flag"
	"fmt"
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
		controller = flag.String("controller", "", "controller address, host:port (required)")
		token      = flag.String("join", "", "one-time join token (first run only)")
		node       = flag.String("node", daemon.Hostname(), "this node's name")
		serverName = flag.String("server-name", "", "expected controller certificate name (default: host part of -controller)")
		verbose    = flag.Bool("v", false, "verbose logging")
	)
	flag.Parse()

	lvl := slog.LevelInfo
	if *verbose {
		lvl = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))

	if *controller == "" {
		fmt.Fprintln(os.Stderr, "shomed: -controller HOST:PORT is required")
		flag.Usage()
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	daemon.WritePID(*root)
	defer daemon.ClearPID(*root)

	if err := daemon.RunAgent(ctx, daemon.AgentConfig{
		Root: *root, Controller: *controller, Token: *token,
		Node: *node, ServerName: *serverName, Log: log,
	}); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}
