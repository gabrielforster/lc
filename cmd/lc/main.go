// Command lc is the agent. It runs on the machine behind NAT, dials out to an
// lcd server and exposes local services through it.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/gabrielforster/lc/internal/agent"
	"github.com/gabrielforster/lc/internal/muxproto"
)

// config is the agent's on-disk configuration. JSON keeps it to the standard
// library; the field names match the control protocol.
type config struct {
	Server  string                `json:"server"`
	Token   string                `json:"token"`
	Tunnels []muxproto.TunnelSpec `json:"tunnels"`
}

func main() {
	fs := flag.NewFlagSet("lc", flag.ExitOnError)
	var (
		path  = fs.String("config", "lc.json", "path to the agent config file")
		debug = fs.Bool("debug", false, "verbose logging")
	)
	fs.Parse(os.Args[1:])

	if err := run(*path, *debug); err != nil {
		fmt.Fprintln(os.Stderr, "lc:", err)
		os.Exit(1)
	}
}

func run(path string, debug bool) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var cfg config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("parsing %s: %w", path, err)
	}
	if cfg.Server == "" || cfg.Token == "" {
		return fmt.Errorf("%s needs both \"server\" and \"token\"", path)
	}

	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	a := agent.New(agent.Config{
		ServerAddr: cfg.Server,
		Token:      cfg.Token,
		Tunnels:    cfg.Tunnels,
	}, log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Info("connecting", "server", cfg.Server, "tunnels", len(cfg.Tunnels))
	return a.Run(ctx)
}
