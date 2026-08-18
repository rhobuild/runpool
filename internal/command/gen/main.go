// Command gen writes the CLI reference and the shell completions from
// the command tree itself, so the documentation cannot drift from what
// the binary does. Run it with `go generate ./internal/command/...`.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rhobuild/runpool/internal/command"
	"github.com/spf13/cobra/doc"
)

func main() {
	// go generate runs in the package directory, so the repository root
	// is found rather than assumed: writing relative to the caller's
	// directory once produced a whole documentation tree nested inside
	// internal/command.
	rootDir, err := repoRoot()
	if err != nil {
		fail(err)
	}
	if err := os.Chdir(rootDir); err != nil {
		fail(err)
	}

	root := command.NewRootCommand(command.BuildInfo{Version: "dev"}, command.IO{
		In: os.Stdin, Out: os.Stdout, Err: os.Stderr,
	})
	root.DisableAutoGenTag = true

	refDir := filepath.Join("docs", "reference", "cli")
	if err := os.MkdirAll(refDir, 0o755); err != nil {
		fail(err)
	}
	// Remove what a previous run wrote, so a command that no longer
	// exists does not linger as documentation for something absent.
	entries, err := filepath.Glob(filepath.Join(refDir, "runpool*.md"))
	if err != nil {
		fail(err)
	}
	for _, e := range entries {
		if err := os.Remove(e); err != nil {
			fail(err)
		}
	}
	if err := doc.GenMarkdownTree(root, refDir); err != nil {
		fail(err)
	}
	generated, err := filepath.Glob(filepath.Join(refDir, "runpool*.md"))
	if err != nil {
		fail(err)
	}
	for _, path := range generated {
		if err := normalizeMarkdown(path); err != nil {
			fail(err)
		}
	}

	compDir := filepath.Join("dist", "completions")
	if err := os.MkdirAll(compDir, 0o755); err != nil {
		fail(err)
	}
	for name, gen := range map[string]func(string) error{
		"runpool.bash": root.GenBashCompletionFile,
		"runpool.zsh":  root.GenZshCompletionFile,
		"runpool.fish": func(p string) error { return root.GenFishCompletionFile(p, true) },
	} {
		if err := gen(filepath.Join(compDir, name)); err != nil {
			fail(err)
		}
	}
	fmt.Println("wrote the CLI reference to", refDir, "and completions to", compDir)
}

// normalizeMarkdown removes generator-specific trailing whitespace and leaves
// exactly one final newline. The generated reference is committed and must pass
// the same repository hygiene checks as handwritten documentation.
func normalizeMarkdown(path string) error {
	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t")
	}
	normalized := strings.TrimRight(strings.Join(lines, "\n"), "\n") + "\n"
	return os.WriteFile(path, []byte(normalized), 0o644)
}

// repoRoot walks up until it finds the module definition.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod above %s", dir)
		}
		dir = parent
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "gen:", err)
	os.Exit(1)
}
