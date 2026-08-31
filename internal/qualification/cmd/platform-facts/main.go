// Command platform-facts emits the local host facts used by release
// qualification. It collects evidence only; verify-platform separately compares
// the document with the reviewed reference.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/rhobuild/runpool/internal/qualification/hostfacts"
)

func main() {
	if len(os.Args) != 1 {
		fmt.Fprintln(os.Stderr, "platform-facts: unexpected arguments")
		os.Exit(2)
	}
	document, err := hostfacts.Collect(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, "platform-facts:", err)
		os.Exit(1)
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(document); err != nil {
		fmt.Fprintln(os.Stderr, "platform-facts: encode evidence:", err)
		os.Exit(1)
	}
}
