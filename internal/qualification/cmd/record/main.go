// Command record assembles the release-qualification record from the
// evidence a qualification run collected and prints it as JSON.
//
// It reads the reviewed platform reference the controller itself
// embeds, so the entry a release is qualified against is the entry the
// product ships with, rather than a second reading of the same file.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/rhobuild/runpool/internal/platform"
	"github.com/rhobuild/runpool/internal/qualification"
)

func main() {
	evidence := flag.String("evidence", "evidence", "directory holding the collected evidence")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "record: unexpected positional arguments")
		os.Exit(2)
	}

	build := qualification.Build{
		Commit:          os.Getenv("COMMIT"),
		ControllerImage: os.Getenv("CONTROLLER_IMAGE"),
		CapsuleImage:    os.Getenv("CAPSULE_IMAGE"),
		Run:             os.Getenv("RUN_URL"),
	}
	for name, value := range map[string]string{
		"COMMIT":           build.Commit,
		"CONTROLLER_IMAGE": build.ControllerImage,
		"CAPSULE_IMAGE":    build.CapsuleImage,
		"RUN_URL":          build.Run,
	} {
		if value == "" {
			fmt.Fprintf(os.Stderr, "record: %s is empty, so the record would name nothing\n", name)
			os.Exit(2)
		}
	}

	reference, err := platform.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "record:", err)
		os.Exit(1)
	}
	document, err := qualification.Assemble(reference, *evidence, build, time.Now())
	if err != nil {
		fmt.Fprintln(os.Stderr, "record:", err)
		os.Exit(1)
	}
	encoded, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "record:", err)
		os.Exit(1)
	}
	fmt.Println(string(encoded))
}
