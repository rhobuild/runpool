// Command image-lock answers the two questions the reviewed image lock is
// asked from outside a Go program.
//
// Without flags it re-resolves every tag in the lock against its registry
// and fails if a digest has drifted, then checks that every platform the
// lock says a release builds for is one the image actually publishes. The
// lock is what the controller executes, so drift means the privileged
// payload changed under a name already reviewed — and a digest bump that
// quietly drops a platform leaves the declared list and the buildable list
// agreeing with each other while agreeing with nothing upstream. This is
// the one check that looks at a registry rather than at another list, and
// it needs network and `docker buildx`.
//
// With -ref it prints one entry's digest-qualified reference and exits,
// which is how a contract script starts a container from exactly the bytes
// the capsule is built from.
//
// The hermetic half — the embedded copy matching the reviewed copy, and the
// declared list matching what the code will build — is in
// internal/imagelock and runs with the ordinary suite.
//
// Usage:
//
//	go run ./internal/qualification/cmd/image-lock
//	go run ./internal/qualification/cmd/image-lock -ref runner
package main

import (
	"errors"
	"flag"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/rhobuild/runpool/internal/imagelock"
)

// The reviewed file rather than the copy this command embeds through the
// package: what is re-resolved against a registry has to be the document a
// human audited. That the two are the same bytes is a separate assertion,
// and it stays separate.
const reviewedLock = "build/images.lock.json"

func main() {
	ref := flag.String("ref", "", "print the digest-qualified reference for one locked image and exit")
	flag.Parse()

	err := run(*ref)
	if errors.Is(err, errFailed) {
		// Every failure has already named itself on stdout, beside the
		// entries that passed. A second summary line would say less.
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ref string) error {
	body, err := os.ReadFile(reviewedLock)
	if err != nil {
		return err
	}
	lock, err := imagelock.Parse(body)
	if err != nil {
		return err
	}
	if ref != "" {
		pinned, err := lock.Pinned(ref)
		if err != nil {
			return err
		}
		fmt.Println(pinned)
		return nil
	}
	return verify(lock, inspect)
}

// verify reports every entry rather than stopping at the first failure: a
// maintainer resolving lock drift wants the whole picture, and a second
// registry round trip to find the next problem is a slow way to get it.
func verify(lock imagelock.Lock, inspect func(ref, format string) (string, error)) error {
	if len(lock.Images) == 0 {
		return fmt.Errorf("%s declares no image, so this proves nothing", reviewedLock)
	}
	failed := false
	for _, name := range slices.Sorted(maps.Keys(lock.Images)) {
		entry := lock.Images[name]
		pinned, err := lock.Pinned(name)
		if err != nil {
			fmt.Printf("FAIL  %s: %v\n", name, err)
			failed = true
			continue
		}
		got, err := inspect(entry.Ref, "{{json .Manifest.Digest}}")
		if err != nil {
			fmt.Printf("FAIL  %s (%s): registry unreachable, so the digest could not be verified\n", name, entry.Ref)
			failed = true
			continue
		}
		if got := strings.Trim(got, `"`); got != entry.Digest {
			fmt.Printf("FAIL  %s %s: lock says %s, registry says %s\n", name, entry.Ref, entry.Digest, got)
			failed = true
			continue
		}
		fmt.Printf("ok    %s %s\n", name, entry.Ref)

		// The digest is right; now the platforms behind it. By digest
		// rather than by tag, so these are the bytes just verified and
		// not whatever the tag resolves to a moment later. A Go template
		// answers this, so nothing here parses a manifest: attestation
		// manifests carry no architecture and are skipped rather than
		// reported as a platform nobody asked for.
		published, err := inspect(pinned, platformTemplate)
		if err != nil {
			fmt.Printf("FAIL  %s (%s): the published platforms could not be read\n", name, entry.Ref)
			failed = true
			continue
		}
		serves := strings.Fields(published)
		for _, want := range lock.Platforms {
			if !slices.Contains(serves, want) {
				fmt.Printf("FAIL  %s %s: the lock builds for %s and the image publishes %s\n",
					name, entry.Ref, want, strings.Join(serves, " "))
				failed = true
			}
		}
	}
	if failed {
		return errFailed
	}
	return nil
}

const platformTemplate = `{{range .Manifest.Manifests}}{{if .Platform}}` +
	`{{if ne .Platform.Architecture "unknown"}}{{.Platform.OS}}/{{.Platform.Architecture}} {{end}}` +
	`{{end}}{{end}}`

func inspect(ref, format string) (string, error) {
	out, err := exec.Command("docker", "buildx", "imagetools", "inspect", ref, "--format", format).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// errFailed carries the exit status for a run whose failures have
// already been printed, so main can exit without repeating them.
var errFailed = errors.New("the image lock does not match its registry")
