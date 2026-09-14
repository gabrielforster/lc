package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/gabrielforster/lc/internal/agent"
	"github.com/gabrielforster/lc/internal/muxproto"
)

// domains implements the one-shot `lc domains ...` commands.
//
// They exist so the claim flow is usable before any UI does: each connects a
// short-lived session, issues one control request and exits.
func domains(cfg config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: lc domains <claim|list|release> [domain]")
	}

	// A one-shot command registers no tunnels; it only needs the control stream.
	a := agent.New(agent.Config{
		ServerAddr: cfg.Server,
		Token:      cfg.Token,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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

	switch args[0] {
	case "claim":
		if len(args) < 2 {
			return fmt.Errorf("usage: lc domains claim <domain>")
		}
		res, err := a.ClaimDomain(args[1])
		if err != nil {
			return err
		}
		if !res.OK {
			return fmt.Errorf("claim refused: %s", explain(res.Reason, args[1]))
		}
		fmt.Printf("claimed %s\n", args[1])
		if res.NeedsDNS {
			// Claiming only records ownership on the server; nothing resolves
			// until DNS points here, and no certificate can be issued either.
			fmt.Printf("\nPoint %s at this server's address before it will resolve.\n", args[1])
		}
		return nil

	case "list":
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

	case "release":
		if len(args) < 2 {
			return fmt.Errorf("usage: lc domains release <domain>")
		}
		res, err := a.ReleaseDomain(args[1])
		if err != nil {
			return err
		}
		if !res.OK {
			return fmt.Errorf("release failed: %s", explain(res.Reason, args[1]))
		}
		fmt.Printf("released %s\n", args[1])
		return nil
	}
	return fmt.Errorf("unknown domains command %q", args[0])
}

// explain turns a typed reason into something worth reading.
func explain(r muxproto.ClaimReason, domain string) string {
	switch r {
	case muxproto.ClaimTaken:
		return fmt.Sprintf("%s is already claimed by another token", domain)
	case muxproto.ClaimReserved:
		return fmt.Sprintf("%s is reserved by the server", domain)
	case muxproto.ClaimDisabled:
		return "this server was not started with -allow-custom-domains"
	case muxproto.ClaimNotFound:
		return fmt.Sprintf("you do not own %s", domain)
	case muxproto.ClaimInvalid:
		return fmt.Sprintf("%q is not a usable hostname", domain)
	}
	return string(r)
}
