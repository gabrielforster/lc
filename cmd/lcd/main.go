// Command lcd is the server daemon. It runs on a host with a public address and
// accepts tunnel sessions from agents behind NAT.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/gabrielforster/lc/internal/registry"
	"github.com/gabrielforster/lc/internal/server"
	"github.com/gabrielforster/lc/internal/sniff"
	"github.com/gabrielforster/lc/internal/store"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "admin" {
		if err := admin(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "lcd admin:", err)
			os.Exit(1)
		}
		return
	}
	if err := serve(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "lcd:", err)
		os.Exit(1)
	}
}

func serve(args []string) error {
	fs := flag.NewFlagSet("lcd", flag.ExitOnError)
	var (
		dbPath    = fs.String("db", "lc.db", "path to the SQLite state file")
		control   = fs.String("control", ":7000", "control listener address")
		httpAddr  = fs.String("http", ":8080", "public HTTP listener address, empty to disable")
		portMin   = fs.Int("port-min", 20000, "lowest automatically assigned public port")
		portMax   = fs.Int("port-max", 20100, "highest automatically assigned public port")
		public    = fs.String("public-host", "127.0.0.1", "hostname users reach this server on")
		custom    = fs.Bool("allow-custom-domains", false, "let agents claim unreserved hostnames")
		resHost   = fs.String("reserved-hosts", "", "comma-separated hostnames the server keeps for itself")
		httpsAddr = fs.String("https", "", "public HTTPS listener address, empty to disable")
		tlsMode   = fs.String("tls", "autocert", "certificate source: autocert | files | selfsigned")
		certFile  = fs.String("tls-cert", "", "certificate file, for -tls=files")
		keyFile   = fs.String("tls-key", "", "private key file, for -tls=files")
		certCache = fs.String("tls-cache", "lc-certs", "certificate cache directory, for -tls=autocert")
		mcAddr    = fs.String("minecraft", "", "public Minecraft listener address, e.g. :25565, empty to disable")
		maxConns  = fs.Int("max-conns", 256, "concurrent public connections allowed per tunnel, 0 for unlimited")
		debug     = fs.Bool("debug", false, "verbose logging")
	)
	fs.Parse(args)

	log := newLogger(*debug)

	db, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	reg := registry.New(db, registry.Config{
		PortMin:            *portMin,
		PortMax:            *portMax,
		AllowCustomDomains: *custom,
		ReservedHosts:      splitList(*resHost),
		PublicHost:         *public,
		MaxConnsPerTunnel:  *maxConns,
	})

	ln, err := net.Listen("tcp", *control)
	if err != nil {
		return err
	}
	defer ln.Close()
	log.Info("control listener up", "addr", ln.Addr().String(),
		"custom_domains", *custom, "port_range", fmt.Sprintf("%d-%d", *portMin, *portMax))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := server.New(reg, log)

	if *httpAddr != "" {
		hln, err := net.Listen("tcp", *httpAddr)
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

	if *httpsAddr != "" {
		certs, err := certSource(*tlsMode, *certFile, *keyFile, *certCache, reg, log)
		if err != nil {
			return err
		}
		sln, err := net.Listen("tcp", *httpsAddr)
		if err != nil {
			return err
		}
		defer sln.Close()
		log.Info("https listener up", "addr", sln.Addr().String(), "tls", *tlsMode)
		go func() {
			if err := srv.ServeHTTPSListener(ctx, sln, certs); err != nil {
				log.Error("https listener failed", "err", err)
			}
		}()
	}

	if *mcAddr != "" {
		// One listener serves every Minecraft tunnel: the hostname the player
		// typed is in the handshake, so the connection can be routed by peeking
		// at it.
		mln, err := net.Listen("tcp", *mcAddr)
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
func certSource(mode, cert, key, cache string, reg *registry.Registry, log *slog.Logger) (server.CertSource, error) {
	switch mode {
	case "files":
		if cert == "" || key == "" {
			return nil, fmt.Errorf("-tls=files needs -tls-cert and -tls-key")
		}
		return server.FileCerts{CertFile: cert, KeyFile: key}, nil

	case "selfsigned":
		// Development only: clients must be told to trust the generated CA.
		log.Warn("serving self-signed certificates; clients will not trust them by default")
		return server.NewSelfSigned()

	case "autocert":
		// The allowlist is consulted per handshake against live claims, so a
		// certificate can only be requested for a name some token owns.
		return server.NewAutocert(cache, func(host string) bool {
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
	return nil, fmt.Errorf("unknown -tls mode %q", mode)
}

func newLogger(debug bool) *slog.Logger {
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
