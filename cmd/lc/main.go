// Command lc is the agent. It runs on the machine behind NAT, dials out to an
// lcd server and exposes local services through it.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/gabrielforster/lc/internal/agent"
	"github.com/gabrielforster/lc/internal/muxproto"
)

// config is the agent's on-disk configuration. JSON keeps it to the standard
// library; the field names match the control protocol.
type config struct {
	Server  string                `json:"server"`
	Token   string                `json:"token"`
	Tunnels []muxproto.TunnelSpec `json:"tunnels"`
	// IdleTimeout, a Go duration such as "15m", closes connections to local
	// services after that long without traffic. The server enforces its own on
	// the public side, so this only covers a local service that holds on after
	// the server goes away.
	IdleTimeout string `json:"idle_timeout,omitempty"`
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "lc:", err)
		hintDoubleDash(os.Stderr, os.Args[1:])
		os.Exit(1)
	}
}

// hintDoubleDash covers the one breaking change in moving to cobra: pflag wants
// two dashes, and "unknown shorthand flag" does not say so.
func hintDoubleDash(w io.Writer, args []string) {
	for _, a := range args {
		if len(a) > 2 && a[0] == '-' && a[1] != '-' {
			fmt.Fprintf(w, "\nFlags now take two dashes: --%s\n", strings.TrimLeft(a, "-"))
			return
		}
	}
}

func newRootCmd() *cobra.Command {
	var (
		path  string
		debug bool
	)

	cmd := &cobra.Command{
		Use:   "lc",
		Short: "Reverse tunnel agent",
		Long: "lc is the agent half of lc. It runs on the machine behind NAT, dials out\n" +
			"to an lcd server and holds one session open for it to push traffic down.",
		Args: cobra.NoArgs,
		// main prints errors, prefixed with the binary name. Usage is worth
		// printing for a bad command line but not for a runtime failure, and by
		// PersistentPreRun cobra has finished parsing.
		SilenceErrors:    true,
		PersistentPreRun: func(cmd *cobra.Command, _ []string) { cmd.Root().SilenceUsage = true },
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := load(path)
			if err != nil {
				return err
			}
			return run(cmd.Context(), cfg, debug)
		},
	}

	// Persistent: the subcommands read the same config file.
	f := cmd.PersistentFlags()
	f.StringVar(&path, "config", "lc.json", "path to the agent config file")
	f.BoolVar(&debug, "debug", false, "verbose logging")

	cmd.AddCommand(newDomainsCmd(&path))
	return cmd
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

// idleTimeout parses the optional duration, treating an empty value as
// "disabled, the server's timeout is enough".
func (c config) idleTimeout() (time.Duration, error) {
	if c.IdleTimeout == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(c.IdleTimeout)
	if err != nil {
		return 0, fmt.Errorf("idle_timeout: %w", err)
	}
	return d, nil
}

func run(parent context.Context, cfg config, debug bool) error {
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	idle, err := cfg.idleTimeout()
	if err != nil {
		return err
	}

	a := agent.New(agent.Config{
		ServerAddr:  cfg.Server,
		Token:       cfg.Token,
		Tunnels:     cfg.Tunnels,
		IdleTimeout: idle,
	}, log)

	// Minecraft tunnels rewrite the handshake so the server sees the player's
	// real address. Every other kind is piped through untouched.
	a.SetTransform(muxproto.KindMinecraft, agent.MinecraftTransform)

	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Info("connecting", "server", cfg.Server, "tunnels", len(cfg.Tunnels))
	return a.Run(ctx)
}
