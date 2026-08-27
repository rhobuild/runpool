package consistency

import (
	"bytes"
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

// denials are the shapes a claim that no release exists takes here, one
// pattern each, paired with a statement it must match.
//
// It is a list and not a parser because the alternative is worse: a
// document is free to discuss SemVer pre-release identifiers, an
// unreleased lease, or a gateway that does not exist yet, and a matcher
// general enough to catch every phrasing catches those too. The cost is
// that a new phrasing has to be added here by hand.
//
// The paired statement is what makes narrowing a pattern safe. Three of
// these were narrowed to clear a false positive, and a pattern narrowed
// past the claim it was written for goes on passing silently; the
// example fails instead.
//
// Words are separated by \s+ rather than a space because the text these
// read has had its line breaks and comment markers blanked, which leaves
// a run of spaces wherever the author wrapped.
var denials = []struct {
	pattern *regexp.Regexp
	matches string
}{
	{regexp.MustCompile(`(?i)runpool\s+is\b[^.,]{0,60}\b(pre-release|unreleased)\b`),
		"Runpool is pre-release infrastructure software"},
	{regexp.MustCompile(`(?i)\b(runpool|the\s+project)\s+has\s+not\s+(been\s+)?released\b`),
		"Runpool has not released: there are no tags"},
	{regexp.MustCompile(`(?i)\bnot\s+yet\s+released\b`),
		"the binary is not yet released"},
	{regexp.MustCompile(`(?i)\bremains\s+unreleased\b`),
		"while the project remains unreleased"},
	{regexp.MustCompile(`(?i)there\s+(is|are)\s+no\s+(release|tag|published)`),
		"There is no release"},
	{regexp.MustCompile(`(?i)\bno\s+release\s+exists\b`),
		"no release exists yet"},
	{regexp.MustCompile(`(?i)\buntil\s+a\s+release\s+exists\b`),
		"cannot be used until a release exists"},
	{regexp.MustCompile(`(?i)\bnothing\b[^.]{0,60}\brelease-qualified\b`),
		"Nothing in this repository is release-qualified or supported yet"},
	{regexp.MustCompile(`(?i)\bnot\s+yet\s+(release-)?qualified\b`),
		"selected for the first qualification, not yet qualified"},
	{regexp.MustCompile(`(?i)\bbefore\s+V1([^.0-9]|$)`),
		"| Engine | Status before V1 |"},
	{regexp.MustCompile(`(?i)\b(qualification|release|project|publication)\s+is\s+blocked\b`),
		"Release qualification is blocked until the host is frozen"},
	{regexp.MustCompile(`(?i)\bpre-release,`),
		"Pre-release, 'upgrade' is: the new binary opens existing state"},
	{regexp.MustCompile(`(?i)\b(until|once|after|before)\s+the\s+first\s+release\b`),
		"Once the first release is published it becomes immutable"},
	{regexp.MustCompile(`(?i)\bnothing\s+has\s+been\s+released\b`),
		"nothing has been released, so 000001_initial is the whole of it"},
	{regexp.MustCompile(`(?i)\buntil\b[^.]{0,60}\bis\s+release-qualified\b`),
		"off by default until controller end-to-end reuse is release-qualified"},
	{regexp.MustCompile(`(?i)\brelease\s+qualification\s+pending\b`),
		"accepted and implemented; release qualification pending"},
	{regexp.MustCompile(`(?i)\bqualification\s+(remains|is\s+still)\s+required\b`),
		"controller E2E qualification remains required"},
	{regexp.MustCompile(`(?i)\bstill\s+needs\s+release\s+qualification\b`),
		"asserts the sentinels and still needs release qualification"},
	{regexp.MustCompile(`(?i)\bqualification\s+not\s+executed\b`),
		"Implemented; qualification not executed"},
	{regexp.MustCompile(`(?i)\bremains\s+unqualified\b`),
		"remains unqualified until the release workflow succeeds"},
	{regexp.MustCompile(`(?i)\b(is|are|but|and)\s+not\s+release-qualified\b`),
		"implemented but not release-qualified"},
	{regexp.MustCompile(`(?i)\bbaseline\s+is\s+still\s+(edited\s+in\s+place|mutable)\b`),
		"a database this build cannot account for while the baseline is still\n// mutable"},
	{regexp.MustCompile(`(?i)\bqualification\b[^.]{0,40}\bhas\s+not\s+run\b`),
		"release qualification on the reference platform has not run"},
	{regexp.MustCompile(`(?i)\bhas\s+not\s+run\s+in\s+release\s+qualification\b`),
		"a suite that has not run in release qualification"},
	{regexp.MustCompile(`status-pre--release`),
		"[![Status](https://img.shields.io/badge/status-pre--release-orange)]"},
}

// TestEveryDenialPatternMatchesTheClaimItIsFor: a pattern that no longer
// matches passes every file, and reads exactly like one that has nothing
// to find.
func TestEveryDenialPatternMatchesTheClaimItIsFor(t *testing.T) {
	for _, denial := range denials {
		if !denial.pattern.MatchString(flatten([]byte(denial.matches))) {
			t.Errorf("%s no longer matches %q", denial.pattern, denial.matches)
		}
	}
}

// TestNoDocumentSaysThereIsNoRelease: the tag freezes every document at
// once, so one of them announcing a release while another denies it is a
// contradiction no later commit can reach.
//
// What a reader observes is whichever they open first, and these are the
// pages they open first: the landing page's banner, the vulnerability
// policy's supported-versions section, the product contract the changelog
// links to, and the error the canonical Compose file prints at an
// operator trying to deploy.
//
// The release workflow cannot hold this. It holds the tag against the
// changelog because only it sees a tag; a tree agreeing with itself
// needs no tag, so it is held here, where it runs on every change.
//
// Every tracked file, not the Markdown alone: the claim also lives in a
// drill script's step banner, in Go comments, in a workflow fixture and
// in the platform lock's own preamble, each of which a reader reaches.
// This file is the exception, because it holds the list.
func TestNoDocumentSaysThereIsNoRelease(t *testing.T) {
	self := "test/consistency/documentation_test.go"
	for _, file := range tracked(t, ".") {
		if file == self {
			continue
		}
		body, err := os.ReadFile(repoPath(file))
		if err != nil {
			t.Fatal(err)
		}
		// Binary content holds no sentences, and go.sum holds hashes
		// that a substring match reads as words.
		if bytes.IndexByte(body, 0) >= 0 || file == "go.sum" {
			continue
		}
		flat := flatten(body)
		for _, denial := range denials {
			for _, at := range denial.pattern.FindAllStringIndex(flat, -1) {
				t.Errorf("%s:%d says there is no release: %s", file,
					lineOf(string(body), at[0]), strings.Join(strings.Fields(flat[at[0]:at[1]]), " "))
			}
		}
	}
}

// wrapMarker is what a wrapped claim carries between its words at a line
// boundary: a comment's continuation prefix in Go, YAML, shell and SQL, a
// Markdown blockquote's bar, and the quotes and comma that separate two
// entries of a JSON string array.
var wrapMarker = regexp.MustCompile(`(?m)^[ \t]*(//+|#+|--+|\*|>+|")[ \t]?|",[ \t]*$`)

// flatten is the text with its line breaks and comment markers blanked,
// so a sentence reads the same whether the author wrapped it or not.
//
// A claim inside a comment block carries the next line's marker between
// its words — "// " in Go, "# " in YAML and shell, "-- " in SQL — which
// is enough for every multi-word pattern to miss it, and comments are
// most of what the whole-tree sweep added. A Markdown callout wraps
// behind "> ", and two entries of a JSON string array are separated by
// a quote, a comma and another quote. Every replacement is the same
// length as what it replaces, so an offset into the result is an offset
// into the file and lineOf still names the right line.
func flatten(body []byte) string {
	blanked := wrapMarker.ReplaceAllFunc(body, func(marker []byte) []byte {
		return bytes.Repeat([]byte{' '}, len(marker))
	})
	return strings.ReplaceAll(string(blanked), "\n", " ")
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
