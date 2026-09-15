// Command lcd is the server daemon. It runs on a host with a public address and
// accepts tunnel sessions from agents behind NAT.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/gabrielforster/lc/internal/registry"
	"github.com/gabrielforster/lc/internal/server"
	"github.com/gabrielforster/lc/internal/sniff"
	"github.com/gabrielforster/lc/internal/store"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "lcd:", err)
		os.Exit(1)
	}
}

// serveOpts collects the server flags. Keeping them in one struct rather than a
// pile of package-level pointers means the command can be built more than once,
// which is what makes it testable.
type serveOpts struct {
	dbPath    string
	control   string
	httpAddr  string
	httpsAddr string
	mcAddr    string
	portMin   int
	portMax   int
	public    string
	custom    bool
	resHosts  []string
	tlsMode   string
	certFile  string
	keyFile   string
	certCache string
	maxConns  int
	idleTO    time.Duration
	debug     bool
}

func newRootCmd() *cobra.Command {
	var o serveOpts

	cmd := &cobra.Command{
		Use:   "lcd",
		Short: "Reverse tunnel server",
		Long: "lcd is the server half of lc. It runs on a host with a public address,\n" +
			"accepts sessions from agents behind NAT, and routes public traffic down them.",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return serve(cmd.Context(), o)
		},
	}

	f := cmd.Flags()
	f.StringVar(&o.dbPath, "db", "lc.db", "path to the SQLite state file")
	f.StringVar(&o.control, "control", ":7000", "control listener address")
	f.StringVar(&o.httpAddr, "http", ":8080", "public HTTP listener address, empty to disable")
	f.StringVar(&o.httpsAddr, "https", "", "public HTTPS listener address, empty to disable")
	f.StringVar(&o.mcAddr, "minecraft", "", "public Minecraft listener address, e.g. :25565, empty to disable")
	f.IntVar(&o.portMin, "port-min", 20000, "lowest automatically assigned public port")
	f.IntVar(&o.portMax, "port-max", 20100, "highest automatically assigned public port")
	f.StringVar(&o.public, "public-host", "127.0.0.1", "hostname users reach this server on")
	f.BoolVar(&o.custom, "allow-custom-domains", false, "let agents claim unreserved hostnames")
	f.StringSliceVar(&o.resHosts, "reserved-hosts", nil, "comma-separated hostnames the server keeps for itself")
	f.StringVar(&o.tlsMode, "tls", "autocert", "certificate source: autocert | files | selfsigned")
	f.StringVar(&o.certFile, "tls-cert", "", "certificate file, for --tls=files")
	f.StringVar(&o.keyFile, "tls-key", "", "private key file, for --tls=files")
	f.StringVar(&o.certCache, "tls-cache", "lc-certs", "certificate cache directory, for --tls=autocert")
	f.IntVar(&o.maxConns, "max-conns", 256, "concurrent public connections allowed per tunnel, 0 for unlimited")
	f.DurationVar(&o.idleTO, "idle-timeout", 15*time.Minute, "close proxied connections after this long without traffic, 0 to disable")
	f.BoolVar(&o.debug, "debug", false, "verbose logging")

	cmd.RegisterFlagCompletionFunc("tls", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return []string{"autocert", "files", "selfsigned"}, cobra.ShellCompDirectiveNoFileComp
	})

	cmd.AddCommand(newAdminCmd())
	return cmd
}

func serve(parent context.Context, o serveOpts) error {
	log := newLogger(o.debug)

	db, err := store.Open(o.dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	reg := registry.New(db, registry.Config{
		PortMin:            o.portMin,
		PortMax:            o.portMax,
		AllowCustomDomains: o.custom,
		ReservedHosts:      trimList(o.resHosts),
		PublicHost:         o.public,
		MaxConnsPerTunnel:  o.maxConns,
		IdleTimeout:        o.idleTO,
	})

	ln, err := net.Listen("tcp", o.control)
	if err != nil {
		return err
	}
	defer ln.Close()
	log.Info("control listener up", "addr", ln.Addr().String(),
		"custom_domains", o.custom, "port_range", fmt.Sprintf("%d-%d", o.portMin, o.portMax))

	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := server.New(reg, log)

	if o.httpAddr != "" {
		hln, err := net.Listen("tcp", o.httpAddr)
		if err != nil {
			return err
		}
		defer hln.Close()
		log.Info("http listener up", "addr", hln.Addr().String())
		go func() {
			if err := srv.ServeHTTPListener(ctx, hln); err != nil {
				log.Error("http listener failed", "err", err)
			}
		}()
	}

	if o.httpsAddr != "" {
		certs, err := certSource(o, reg, log)
		if err != nil {
			return err
		}
		sln, err := net.Listen("tcp", o.httpsAddr)
		if err != nil {
			return err
		}
		defer sln.Close()
		log.Info("https listener up", "addr", sln.Addr().String(), "tls", o.tlsMode)
		go func() {
			if err := srv.ServeHTTPSListener(ctx, sln, certs); err != nil {
				log.Error("https listener failed", "err", err)
			}
		}()
	}

	if o.mcAddr != "" {
		// One listener serves every Minecraft tunnel: the hostname the player
		// typed is in the handshake, so the connection can be routed by peeking
		// at it.
		mln, err := net.Listen("tcp", o.mcAddr)
		if err != nil {
			return err
		}
		defer mln.Close()
		log.Info("minecraft listener up", "addr", mln.Addr().String())
		go func() {
			if err := srv.ServeSniffed(ctx, mln, sniff.Minecraft{}); err != nil {
				log.Error("minecraft listener failed", "err", err)
			}
		}()
	}

	return srv.ServeControl(ctx, ln)
}

// certSource builds the configured certificate source.
func certSource(o serveOpts, reg *registry.Registry, log *slog.Logger) (server.CertSource, error) {
	switch o.tlsMode {
	case "files":
		if o.certFile == "" || o.keyFile == "" {
			return nil, fmt.Errorf("--tls=files needs --tls-cert and --tls-key")
		}
		return server.FileCerts{CertFile: o.certFile, KeyFile: o.keyFile}, nil

	case "selfsigned":
		// Development only: clients must be told to trust the generated CA.
		log.Warn("serving self-signed certificates; clients will not trust them by default")
		return server.NewSelfSigned()

	case "autocert":
		// The allowlist is consulted per handshake against live claims, so a
		// certificate can only be requested for a name some token owns.
		return server.NewAutocert(o.certCache, func(host string) bool {
			domains, err := reg.AllowedDomains()
			if err != nil {
				log.Error("checking domain allowlist", "err", err)
				return false
			}
			for _, d := range domains {
				if d == host {
					return true
				}
			}
			return false
		}), nil
	}
	return nil, fmt.Errorf("unknown --tls mode %q", o.tlsMode)
}

func newLogger(debug bool) *slog.Logger {
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

// trimList drops blank entries, so --reserved-hosts="" and a trailing comma are
// both harmless.
func trimList(in []string) []string {
	var out []string
	for _, p := range in {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
