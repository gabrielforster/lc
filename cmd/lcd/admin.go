package main

import (
	"fmt"
	"os"
	"strconv"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/gabrielforster/lc/internal/store"
)

// The admin commands are the stopgap for managing tokens and grants until the
// UI exists. They write the same SQLite file the daemon reads, which is the
// whole point of keeping state there.
func newAdminCmd() *cobra.Command {
	var dbPath string

	cmd := &cobra.Command{
		Use:   "admin",
		Short: "Manage tokens and grants in the state file",
		Long: "Mint credentials and decide what each one may claim.\n\n" +
			"These commands edit the SQLite file directly, so they work whether or not\n" +
			"the daemon is running.",
	}
	// Persistent, so it can be given before or after the subcommand.
	cmd.PersistentFlags().StringVar(&dbPath, "db", "lc.db", "path to the SQLite state file")

	cmd.AddCommand(
		newTokenCmd(&dbPath),
		newGrantCmd(&dbPath),
		newListCmd(&dbPath),
	)
	return cmd
}

// open is shared by every admin subcommand: they all want the same database and
// all want it closed again.
func open(dbPath string, fn func(*store.DB) error) error {
	db, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	return fn(db)
}

func newTokenCmd(dbPath *string) *cobra.Command {
	var label string

	cmd := &cobra.Command{
		Use:   "token",
		Short: "Mint a new token",
		Long:  "Creates a credential and prints its secret once. Only the hash is stored.",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return open(*dbPath, func(db *store.DB) error {
				tok, secret, err := db.CreateToken(label)
				if err != nil {
					return err
				}
				// The secret is shown once: only its hash is stored.
				fmt.Printf("token id: %d\nsecret:   %s\n\nThis secret is not recoverable; store it now.\n", tok.ID, secret)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&label, "label", "", "human-readable label")
	return cmd
}

func newGrantCmd(dbPath *string) *cobra.Command {
	var (
		id    int64
		kind  string
		value string
	)

	cmd := &cobra.Command{
		Use:   "grant",
		Short: "Allow a token to claim a host, zone or port",
		Example: "  lcd admin grant --token 1 --kind port_auto\n" +
			"  lcd admin grant --token 1 --kind wildcard --value .mc.example.com\n" +
			"  lcd admin grant --token 1 --kind host --value play.example.com",
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return open(*dbPath, func(db *store.DB) error {
				if err := db.AddGrant(id, store.GrantKind(kind), value); err != nil {
					return err
				}
				fmt.Printf("granted %s %q to token %d\n", kind, value, id)
				return nil
			})
		},
	}
	f := cmd.Flags()
	f.Int64Var(&id, "token", 0, "token id")
	f.StringVar(&kind, "kind", "", "host | wildcard | port | port_auto")
	f.StringVar(&value, "value", "", "hostname, zone (.mc.example.com) or port")
	cmd.MarkFlagRequired("token")
	cmd.MarkFlagRequired("kind")
	cmd.RegisterFlagCompletionFunc("kind", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return []string{"host", "wildcard", "port", "port_auto"}, cobra.ShellCompDirectiveNoFileComp
	})
	return cmd
}

func newListCmd(dbPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List tokens with their grants, domains and reserved ports",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return open(*dbPath, func(db *store.DB) error {
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
			})
		},
	}
}
