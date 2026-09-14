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

	if err := dispatch(*path, *debug, fs.Args()); err != nil {
		fmt.Fprintln(os.Stderr, "lc:", err)
		os.Exit(1)
	}
}

// dispatch chooses between running the agent and the one-shot subcommands.
func dispatch(path string, debug bool, args []string) error {
	cfg, err := load(path)
	if err != nil {
		return err
	}
	if len(args) > 0 && args[0] == "domains" {
		return domains(cfg, args[1:])
	}
	return run(cfg, debug)
}

func load(path string) (config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return config{}, err
	}
	var cfg config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return config{}, fmt.Errorf("parsing %s: %w", path, err)
	}
	if cfg.Server == "" || cfg.Token == "" {
		return config{}, fmt.Errorf("%s needs both \"server\" and \"token\"", path)
	}
	return cfg, nil
}

func run(cfg config, debug bool) error {
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

	// Minecraft tunnels rewrite the handshake so the server sees the player's
	// real address. Every other kind is piped through untouched.
	a.SetTransform(muxproto.KindMinecraft, agent.MinecraftTransform)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Info("connecting", "server", cfg.Server, "tunnels", len(cfg.Tunnels))
	return a.Run(ctx)
}
