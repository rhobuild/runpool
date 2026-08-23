// Command verify-platform compares collected host facts with Runpool's
// release-qualification reference. It fails closed when a fact is missing
// or differs.
//
// It is the first of the three commands a release is assembled from: this
// one decides whether the host that ran the suites is the host the
// reference describes, record states what was qualified, and verify-record
// checks that record against the build being published.
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
	// The entry for the platform that was measured, not for the platform
	// this happens to be running on: the facts came from a host, and it
	// is that host being qualified.
	//
	// A platform with no entry is not a host that failed. It is one
	// nobody has run the suites on, and it says so rather than reporting
	// an architecture mismatch against whichever entry came first.
	qualified, ok := reference.For(observed.Arch)
	if !ok {
		fmt.Fprintln(os.Stderr, "verify:", reference.NotQualified(observed.Arch))
		os.Exit(1)
	}
	if err := qualified.RequireFrozen(); err != nil {
		fmt.Fprintln(os.Stderr, "verify:", err)
		os.Exit(1)
	}
	if mismatches := qualified.Compare(observed); len(mismatches) != 0 {
		fmt.Fprintf(os.Stderr, "observed %s host does not match the release-qualification reference:\n",
			observed.Arch)
		for _, mismatch := range mismatches {
			fmt.Fprintln(os.Stderr, " -", mismatch)
		}
		os.Exit(1)
	}
	fmt.Printf("release-qualification reference matched: %s %s %s, Docker Engine %s\n",
		qualified.Platform.OS, qualified.Platform.OSVersion,
		qualified.Platform.Arch, qualified.Platform.Engine)
}
