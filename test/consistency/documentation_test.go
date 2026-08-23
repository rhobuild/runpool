package consistency

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	relativeLink = regexp.MustCompile(`\]\(([^)]+)\)`)
	pathShaped   = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)
)

// TestEveryPathTheDocumentationNamesResolves: a rename is only safe if
// something notices the references it leaves behind.
//
// Two forms name a file and only one of them is a link. The shell check
// this replaces read `](target)` and nothing else, so it covered two of
// the documentation's references to scripts and missed every path
// written as prose or as a command — the gate commands in
// CONTRIBUTING.md, the drill invocation in the runbook, the contract
// script in an ADR. Those are the references a rename actually breaks,
// because a link is at least visibly broken on the rendered page while a
// stale path reads as fact.
//
// Markdown structure is not parsed and code blocks are not skipped, on
// purpose: a path inside a fenced example is a path somebody will copy,
// and staleness there is worse rather than exempt. What keeps that from
// flagging prose is the definition of a path claim below, not the
// document's syntax.
//
// It is deliberately offline: external URLs are not fetched, which would
// make the gate flaky and slow. Only the in-repo references a
// reorganization can silently break.
func TestEveryPathTheDocumentationNamesResolves(t *testing.T) {
	// A token is a claim about this repository when its first segment
	// names something at the root. Without that test the check reads
	// `linux/amd64`, `modernc.org/sqlite` and `actions/cache` as paths:
	// measured across the documentation, one token in three that holds a
	// slash is not a path here at all.
	entries, err := os.ReadDir(repoPath())
	if err != nil {
		t.Fatal(err)
	}
	topLevel := map[string]bool{}
	for _, entry := range entries {
		topLevel[entry.Name()] = true
	}

	claims := 0
	for _, file := range trackedDocs(t) {
		body, err := os.ReadFile(repoPath(file))
		if err != nil {
			t.Fatal(err)
		}
		dir := filepath.Dir(file)
		for number, line := range strings.Split(string(body), "\n") {
			for _, match := range relativeLink.FindAllStringSubmatch(line, -1) {
				target := linkTarget(match[1])
				if target == "" {
					continue
				}
				claims++
				if _, err := os.Stat(repoPath(filepath.Join(dir, target))); err != nil {
					t.Error(unresolved(file, number, match[1], "link"))
				}
			}
			for _, token := range strings.Fields(line) {
				target := pathClaim(token, topLevel)
				if target == "" {
					continue
				}
				claims++
				if _, err := os.Stat(repoPath(target)); err != nil {
					t.Error(unresolved(file, number, target, "path"))
				}
			}
		}
	}
	if claims == 0 {
		t.Fatal("the documentation names no in-repo path, so this proves nothing")
	}
}

func unresolved(file string, number int, target, kind string) string {
	return fmt.Sprintf("%s:%d names %s as a %s, and nothing is there",
		file, number+1, target, kind)
}

// linkTarget is the in-repo path a markdown link names, or empty for one
// this check does not follow. A link with a #fragment is checked up to
// the fragment; a bare #fragment is a same-file anchor.
func linkTarget(target string) string {
	target = strings.SplitN(target, "#", 2)[0]
	target = strings.SplitN(target, " ", 2)[0]
	switch {
	case target == "",
		strings.HasPrefix(target, "http://"),
		strings.HasPrefix(target, "https://"),
		strings.HasPrefix(target, "mailto:"):
		return ""
	}
	return target
}

// pathClaim is the repository path a token names, or empty when it is
// something else: a module path, a platform, a container path, a URL, a
// configuration key.
func pathClaim(token string, topLevel map[string]bool) string {
	token = strings.Trim(token, "`*_\"'.,;:!?()[]{}<>")
	token = strings.SplitN(token, "#", 2)[0]
	token = strings.TrimRight(token, "/")
	switch {
	case !strings.Contains(token, "/"),
		strings.Contains(token, "://"),
		strings.HasPrefix(token, "/"),
		!pathShaped.MatchString(token):
		return ""
	}
	if !topLevel[strings.SplitN(token, "/", 2)[0]] {
		return ""
	}
	return token
}

// trackedDocs is every Markdown file git knows about, including ones not
// yet added: a reference is worth checking from the moment it is
// written.
func trackedDocs(t *testing.T) []string {
	t.Helper()
	out, err := exec.Command("git", "-C", repoPath(), "ls-files", "-co",
		"--exclude-standard", "--", "*.md").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	var files []string
	for _, file := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if file != "" {
			files = append(files, file)
		}
	}
	if len(files) < 2 {
		t.Fatalf("the tree holds %d Markdown files, so this proves nothing", len(files))
	}
	return files
}
