// Command seed prepares a state directory for the lifecycle drills: it
// opens the store the way the controller would — creating the instance
// identity and migrating to the current schema — and leaves one audit
// entry as a marker the drills can verify across backup, restore and
// upgrade. It exists because every real path that creates state needs a
// provider credential, and the drills must not.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/rhobuild/runpool/internal/store"
)

func main() {
	if len(os.Args) != 3 || (os.Args[1] != "create" && os.Args[1] != "verify") {
		fmt.Fprintln(os.Stderr, "usage: seed <create|verify> <state-dir>")
		os.Exit(2)
	}
	mode, dir := os.Args[1], os.Args[2]

	st, err := store.Open(dir, store.DefaultRetryBudget)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}
	defer st.Close()

	ctx := context.Background()
	switch mode {
	case "create":
		if err := st.Tx(ctx, func(tx *store.Tx) error {
			return tx.RecordAudit("lifecycle-drill", "seed", "instance", "marker for backup/restore verification")
		}); err != nil {
			fmt.Fprintln(os.Stderr, "seed:", err)
			os.Exit(1)
		}
		fmt.Println(st.InstanceID())
	case "verify":
		var found bool
		if err := st.Tx(ctx, func(tx *store.Tx) error {
			entries, err := tx.AuditTail(50)
			if err != nil {
				return err
			}
			for _, e := range entries {
				if e.Actor == "lifecycle-drill" && e.Action == "seed" {
					found = true
				}
			}
			return nil
		}); err != nil {
			fmt.Fprintln(os.Stderr, "verify:", err)
			os.Exit(1)
		}
		if !found {
			fmt.Fprintln(os.Stderr, "verify: the seeded audit marker is gone; the state did not survive")
			os.Exit(1)
		}
		fmt.Println(st.InstanceID())
	}
}
