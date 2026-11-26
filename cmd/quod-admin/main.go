// Command quod-admin manages server-issued access keys.
//
//	quod-admin keys create --name alice [--daily-budget 2.00]   # prints the key once
//	quod-admin keys list
//	quod-admin keys revoke alice|k_3f9a1c2b
//
// Changes take effect on the running server's next request.
package main

import (
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"mathprover/internal/auth"
)

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  quod-admin keys create --name NAME [--daily-budget USD] [--db quod.db]
  quod-admin keys list   [--db quod.db]
  quod-admin keys revoke ID|NAME [--db quod.db]`)
	os.Exit(2)
}

func main() {
	if len(os.Args) < 3 || os.Args[1] != "keys" {
		usage()
	}
	cmd, args := os.Args[2], os.Args[3:]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	dbPath := fs.String("db", "quod.db", "SQLite database (same as the server's -db)")
	store := func() *auth.KeyStore {
		s, err := auth.OpenKeyStore(*dbPath)
		if err != nil {
			fail(err)
		}
		return s
	}
	switch cmd {
	case "create":
		name := fs.String("name", "", "who the key is for (required)")
		budget := fs.Float64("daily-budget", 0, "USD per UTC day for this key; 0 = only server-wide caps")
		_ = fs.Parse(args)
		s := store()
		defer s.Close()
		secret, k, err := s.Create(*name, *budget)
		if err != nil {
			fail(err)
		}
		// Only the secret goes to stdout, so `quod-admin keys create ... > file` works.
		fmt.Fprintf(os.Stderr, "created %s (%s); this key is shown only once:\n", k.ID, k.Name)
		fmt.Println(secret)
	case "list":
		_ = fs.Parse(args)
		s := store()
		defer s.Close()
		ks, err := s.List()
		if err != nil {
			fail(err)
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tNAME\tCREATED\tDAILY BUDGET\tSTATUS")
		for _, k := range ks {
			status, budget := "active", "-"
			if k.Revoked != nil {
				status = "revoked " + k.Revoked.Format(time.DateOnly)
			}
			if k.DailyBudgetUSD > 0 {
				budget = fmt.Sprintf("$%.2f", k.DailyBudgetUSD)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", k.ID, k.Name, k.Created.Format(time.DateOnly), budget, status)
		}
		tw.Flush()
	case "revoke":
		// accept the target before or after flags
		target := ""
		if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
			target, args = args[0], args[1:]
		}
		_ = fs.Parse(args)
		if target == "" {
			target = fs.Arg(0)
		}
		if target == "" {
			usage()
		}
		s := store()
		defer s.Close()
		k, err := s.Revoke(target)
		if err != nil {
			fail(err)
		}
		fmt.Printf("revoked %s (%s)\n", k.ID, k.Name)
	default:
		usage()
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "quod-admin:", err)
	os.Exit(1)
}
