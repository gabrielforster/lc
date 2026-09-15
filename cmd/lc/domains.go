package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/spf13/cobra"

	"github.com/gabrielforster/lc/internal/agent"
	"github.com/gabrielforster/lc/internal/muxproto"
)

// The domains commands exist so the claim flow is usable before any UI does:
// each connects a short-lived session, issues one control request and exits.
func newDomainsCmd(path *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "domains",
		Short: "Claim, list and release custom domains",
		Long: "Custom domains are claimed at runtime, against a server started with\n" +
			"--allow-custom-domains. A claim is durable: the hostname stays yours\n" +
			"across reconnects until you release it.",
	}
	cmd.AddCommand(
		newClaimCmd(path),
		newDomainsListCmd(path),
		newReleaseCmd(path),
	)
	return cmd
}

func newClaimCmd(path *string) *cobra.Command {
	return &cobra.Command{
		Use:   "claim <domain>",
		Short: "Claim a hostname for this token",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withSession(cmd.Context(), *path, func(a *agent.Agent) error {
				domain := args[0]
				res, err := a.ClaimDomain(domain)
				if err != nil {
					return err
				}
				if !res.OK {
					return fmt.Errorf("claim refused: %s", explain(res.Reason, domain))
				}
				fmt.Printf("claimed %s\n", domain)
				if res.NeedsDNS {
					// A claim records ownership; nothing resolves, and no
					// certificate can be issued, until DNS points here.
					fmt.Printf("\nPoint %s at this server's address before it will resolve.\n", domain)
				}
				return nil
			})
		},
	}
}

func newDomainsListCmd(path *string) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the hostnames this token has claimed",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withSession(cmd.Context(), *path, func(a *agent.Agent) error {
				hosts, err := a.Domains()
				if err != nil {
					return err
				}
				if len(hosts) == 0 {
					fmt.Println("no domains claimed")
					return nil
				}
				for _, h := range hosts {
					fmt.Println(h)
				}
				return nil
			})
		},
	}
}

func newReleaseCmd(path *string) *cobra.Command {
	return &cobra.Command{
		Use:   "release <domain>",
		Short: "Give a claimed hostname back",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withSession(cmd.Context(), *path, func(a *agent.Agent) error {
				domain := args[0]
				res, err := a.ReleaseDomain(domain)
				if err != nil {
					return err
				}
				if !res.OK {
					return fmt.Errorf("release failed: %s", explain(res.Reason, domain))
				}
				fmt.Printf("released %s\n", domain)
				return nil
			})
		},
	}
}

// withSession runs one request over a short-lived session. Registering no
// tunnels keeps it to the control stream.
func withSession(parent context.Context, path string, fn func(*agent.Agent) error) error {
	cfg, err := load(path)
	if err != nil {
		return err
	}

	a := agent.New(agent.Config{
		ServerAddr: cfg.Server,
		Token:      cfg.Token,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()

	errc := make(chan error, 1)
	go func() { errc <- a.Run(ctx) }()

	ready := make(chan error, 1)
	go func() { ready <- a.WaitReady(ctx) }()

	select {
	// A rejected token surfaces as Run returning, which would otherwise leave
	// WaitReady spinning until the timeout.
	case err := <-errc:
		if err != nil {
			return err
		}
		return fmt.Errorf("connection closed before the command could run")
	case err := <-ready:
		if err != nil {
			return fmt.Errorf("connecting to %s: %w", cfg.Server, err)
		}
	}
	return fn(a)
}

// explain turns a typed reason into something worth reading.
func explain(r muxproto.ClaimReason, domain string) string {
	switch r {
	case muxproto.ClaimTaken:
		return fmt.Sprintf("%s is already claimed by another token", domain)
	case muxproto.ClaimReserved:
		return fmt.Sprintf("%s is reserved by the server", domain)
	case muxproto.ClaimDisabled:
		return "this server was not started with --allow-custom-domains"
	case muxproto.ClaimNotFound:
		return fmt.Sprintf("you do not own %s", domain)
	case muxproto.ClaimInvalid:
		return fmt.Sprintf("%q is not a usable hostname", domain)
	}
	return string(r)
}
