// Command gen writes internal/store/schema/current.sql: the schema that
// results from applying every runtime migration to an empty database,
// exported in normalized, deterministic order. The migrations stay the
// single authority — this file is a mechanical projection of them for
// sqlc's code generation, never edited by hand, and a parity test fails
// the build when it drifts from what the migrations actually produce.
//
// Run it from the repository root:
//
//	go run ./internal/store/schema/gen
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/rhobuild/runpool/internal/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "gen:", err)
		os.Exit(1)
	}
}

func run() error {
	dir, err := os.MkdirTemp("", "runpool-schema-gen-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	current, err := store.SchemaProjection(dir)
	if err != nil {
		return err
	}
	out := filepath.Join("internal", "store", "schema", "current.sql")
	if err := os.WriteFile(out, []byte(current), 0o644); err != nil {
		return err
	}
	fmt.Println("wrote", out)
	return nil
}
