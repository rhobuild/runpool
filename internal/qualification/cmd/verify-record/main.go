// Command verify-record checks that a release-qualification record
// covers the build about to be published. It fails closed when the
// record names another commit or another image digest.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/rhobuild/runpool/internal/qualification"
)

func main() {
	recordPath := flag.String("record", "release-qualification.json",
		"path to the release-qualification record")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "verify-record: unexpected positional arguments")
		os.Exit(2)
	}

	body, err := os.ReadFile(*recordPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "verify-record:", err)
		os.Exit(1)
	}
	var document qualification.Record
	if err := json.Unmarshal(body, &document); err != nil {
		fmt.Fprintln(os.Stderr, "verify-record: decode the record:", err)
		os.Exit(1)
	}
	publishing := qualification.Build{
		Commit:          os.Getenv("COMMIT"),
		ControllerImage: os.Getenv("CONTROLLER_IMAGE"),
		CapsuleImage:    os.Getenv("CAPSULE_IMAGE"),
	}
	if err := document.CoversBuild(publishing); err != nil {
		fmt.Fprintln(os.Stderr, "verify-record:", err)
		os.Exit(1)
	}
}
