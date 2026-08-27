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
	for _, file := range tracked(t, "*.md") {
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

// denials are the claims this repository made, one pattern per shape it
// used. It is a list and not a parser, and the reason to prefer that to
// something cleverer is what the cleverer thing did: a single pattern
// for "Runpool is pre-release" passed green over six documents saying a
// release does not exist six other ways, two of them in files the same
// change had just edited. A new way to say it has to be added here,
// which is the point.
var denials = []*regexp.Regexp{
	regexp.MustCompile(`(?i)runpool is\b[^.]{0,60}\b(pre-release|unreleased)\b`),
	regexp.MustCompile(`(?i)\bhas not released\b`),
	regexp.MustCompile(`(?i)\bremains unreleased\b`),
	regexp.MustCompile(`(?i)there (is|are) no (release|tag|published)`),
	regexp.MustCompile(`(?i)\bno release exists\b`),
	regexp.MustCompile(`(?i)\buntil a release exists\b`),
	regexp.MustCompile(`(?i)\bnothing\b[^.]{0,60}\brelease-qualified\b`),
	regexp.MustCompile(`(?i)\bnot yet (release-)?qualified\b`),
	regexp.MustCompile(`(?i)\bbefore V1\b`),
	regexp.MustCompile(`(?i)the project is blocked`),
	regexp.MustCompile(`(?i)\bbefore the first release\b`),
	regexp.MustCompile(`status-pre--release`),
}

// TestNoDocumentSaysThereIsNoRelease: the tag freezes every document at
// once, so one of them announcing a release while another denies it is a
// contradiction no later commit can reach.
//
// What a reader observes is whichever they open first, and the denials
// were on the pages they open first: the landing page's banner, the
// vulnerability policy's supported-versions section, the product
// contract the changelog links them to, and the error the canonical
// Compose file prints when they try to deploy.
//
// The release workflow cannot hold this. It holds the tag against the
// changelog because only it sees a tag; a tree agreeing with itself
// needs no tag, so it is held here, where it runs on every change.
//
// Newlines are spaces here because a claim wrapped between "Runpool is"
// and "pre-release" is the same claim; the substitution is byte for
// byte so the reported line still names where it starts.
func TestNoDocumentSaysThereIsNoRelease(t *testing.T) {
	files := tracked(t, "*.md", "deploy/*")
	for _, file := range files {
		body, err := os.ReadFile(repoPath(file))
		if err != nil {
			t.Fatal(err)
		}
		flat := strings.ReplaceAll(string(body), "\n", " ")
		for _, denial := range denials {
			for _, at := range denial.FindAllStringIndex(flat, -1) {
				t.Errorf("%s:%d says there is no release: %s", file,
					lineOf(string(body), at[0]), flat[at[0]:at[1]])
			}
		}
	}
}

// lineOf is the 1-based line the byte offset falls on.
func lineOf(body string, offset int) int {
	return strings.Count(body[:offset], "\n") + 1
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

// tracked is every file git knows about matching the patterns, including
// ones not yet added: a reference is worth checking from the moment it is
// written.
func tracked(t *testing.T, patterns ...string) []string {
	t.Helper()
	args := append([]string{"-C", repoPath(), "ls-files", "-co",
		"--exclude-standard", "--"}, patterns...)
	out, err := exec.Command("git", args...).Output()
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
		t.Fatalf("the tree holds %d files matching %v, so this proves nothing",
			len(files), patterns)
	}
	return files
}
