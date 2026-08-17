// Command verify compares collected host facts with Runpool's release-
// qualification reference. It fails closed when a fact is missing or differs.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/rhobuild/runpool/internal/platform"
)

func main() {
	observedPath := flag.String("observed", "platform-facts.json", "path to collected platform facts")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "verify: unexpected positional arguments")
		os.Exit(2)
	}

	b, err := os.ReadFile(*observedPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "verify:", err)
		os.Exit(1)
	}
	var observed platform.Facts
	if err := json.Unmarshal(b, &observed); err != nil {
		fmt.Fprintln(os.Stderr, "verify: decode observed platform:", err)
		os.Exit(1)
	}

	reference, err := platform.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "verify:", err)
		os.Exit(1)
	}
	if err := reference.RequireFrozen(); err != nil {
		fmt.Fprintln(os.Stderr, "verify:", err)
		os.Exit(1)
	}
	if mismatches := reference.Compare(observed); len(mismatches) != 0 {
		fmt.Fprintln(os.Stderr, "observed host does not match the release-qualification reference:")
		for _, mismatch := range mismatches {
			fmt.Fprintln(os.Stderr, " -", mismatch)
		}
		os.Exit(1)
	}
	fmt.Printf("release-qualification reference matched: %s %s, Docker Engine %s\n",
		reference.Platform.OS, reference.Platform.OSVersion, reference.Platform.Engine)
}
