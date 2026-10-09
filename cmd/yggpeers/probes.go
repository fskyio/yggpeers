package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	"yggpeers/internal/config"
	"yggpeers/internal/store"
)

const probeUsage = `Usage:
  yggpeers                         Run the main instance (no peer probing)
  yggpeers probes add NAME          Provision a named probe; print token once
  yggpeers probes list              List identities, registration and check-ins
  yggpeers probes disable ID        Disable access immediately; exclude from new rounds
  yggpeers probes enable ID         Re-enable an identity for subsequent rounds
  yggpeers probes rotate ID         Revoke the old credential; print replacement once

Management commands run on the main host and use DB_PATH. Keep emitted tokens secret.
`

func manageProbes(cfg config.Config, args []string) error {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h" || args[0] == "help") {
		fmt.Print(probeUsage)
		return nil
	}
	if len(args) < 2 || args[0] != "probes" {
		return errors.New(probeUsage)
	}
	command := args[1]
	if (command == "list" && len(args) != 2) || (command != "list" && len(args) != 3) {
		return errors.New(probeUsage)
	}
	if command != "list" && command != "add" && command != "disable" && command != "enable" && command != "rotate" {
		return errors.New(probeUsage)
	}
	if err := os.MkdirAll(filepath.Dir(cfg.DBPath), 0o755); err != nil {
		return err
	}
	st, err := store.New(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()
	switch command {
	case "add":
		p, token, err := st.CreateProbe(args[2])
		if err != nil {
			return err
		}
		fmt.Printf("Probe ID: %s\nPROBE_TOKEN=%s\n", p.ID, token)
	case "rotate":
		token, err := st.RotateProbeToken(args[2])
		if err != nil {
			return err
		}
		fmt.Printf("PROBE_TOKEN=%s\n", token)
	case "enable", "disable":
		if err := st.SetProbeEnabled(args[2], command == "enable"); err != nil {
			return err
		}
		fmt.Printf("Probe %s: %sd\n", args[2], command)
	case "list":
		probes, err := st.ListProbes()
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tNAME\tENABLED\tOVERLAY\tREGISTERED\tLAST CHECK-IN (UTC)")
		for _, p := range probes {
			registered := "pending"
			if p.RegisteredAt != nil {
				registered = p.RegisteredAt.UTC().Format(time.RFC3339)
			}
			seen := "never"
			if p.LastSeenAt != nil {
				seen = p.LastSeenAt.UTC().Format(time.RFC3339)
			}
			fmt.Fprintf(w, "%s\t%s\t%t\t%t\t%s\t%s\n", p.ID, p.Name, p.Enabled, p.CheckDarknet, registered, seen)
		}
		return w.Flush()
	}
	return nil
}
