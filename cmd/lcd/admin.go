package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"text/tabwriter"

	"github.com/gabrielforster/lc/internal/store"
)

// admin is the stopgap for managing tokens and grants until the UI exists. It
// writes the same SQLite file the daemon reads, which is the whole point of
// keeping state there.
func admin(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: lcd admin <token|grant|list> [flags]")
	}
	fs := flag.NewFlagSet("admin", flag.ExitOnError)
	dbPath := fs.String("db", "lc.db", "path to the SQLite state file")

	switch args[0] {
	case "token":
		label := fs.String("label", "", "human-readable label")
		fs.Parse(args[1:])
		db, err := store.Open(*dbPath)
		if err != nil {
			return err
		}
		defer db.Close()

		tok, secret, err := db.CreateToken(*label)
		if err != nil {
			return err
		}
		// The secret is shown once: only its hash is stored.
		fmt.Printf("token id: %d\nsecret:   %s\n\nThis secret is not recoverable; store it now.\n", tok.ID, secret)
		return nil

	case "grant":
		id := fs.Int64("token", 0, "token id")
		kind := fs.String("kind", "", "host | wildcard | port | port_auto")
		value := fs.String("value", "", "hostname, zone (.mc.example.com) or port")
		fs.Parse(args[1:])
		if *id == 0 || *kind == "" {
			return fmt.Errorf("grant needs -token and -kind")
		}
		db, err := store.Open(*dbPath)
		if err != nil {
			return err
		}
		defer db.Close()

		if err := db.AddGrant(*id, store.GrantKind(*kind), *value); err != nil {
			return err
		}
		fmt.Printf("granted %s %q to token %d\n", *kind, *value, *id)
		return nil

	case "list":
		fs.Parse(args[1:])
		db, err := store.Open(*dbPath)
		if err != nil {
			return err
		}
		defer db.Close()

		tokens, err := db.ListTokens()
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tLABEL\tSTATE\tGRANTS\tDOMAINS\tPORTS")
		for _, t := range tokens {
			grants, _ := db.Grants(t.ID)
			domains, _ := db.Domains(t.ID)
			ports, _ := db.Ports(t.ID)

			state := "active"
			if t.Disabled {
				state = "disabled"
			}
			var gs, ps string
			for i, g := range grants {
				if i > 0 {
					gs += ","
				}
				gs += string(g.Kind) + ":" + g.Value
			}
			for i, p := range ports {
				if i > 0 {
					ps += ","
				}
				ps += strconv.Itoa(p.Port) + "(" + p.TunnelName + ")"
			}
			fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%v\t%s\n", t.ID, t.Label, state, gs, domains, ps)
		}
		return w.Flush()
	}
	return fmt.Errorf("unknown admin command %q", args[0])
}
