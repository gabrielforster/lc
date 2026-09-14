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
		dbPath  = fs.String("db", "lc.db", "path to the SQLite state file")
		control = fs.String("control", ":7000", "control listener address")
		portMin = fs.Int("port-min", 20000, "lowest automatically assigned public port")
		portMax = fs.Int("port-max", 20100, "highest automatically assigned public port")
		public  = fs.String("public-host", "127.0.0.1", "hostname users reach this server on")
		custom  = fs.Bool("allow-custom-domains", false, "let agents claim unreserved hostnames")
		resHost = fs.String("reserved-hosts", "", "comma-separated hostnames the server keeps for itself")
		debug   = fs.Bool("debug", false, "verbose logging")
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

	return server.New(reg, log).ServeControl(ctx, ln)
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
