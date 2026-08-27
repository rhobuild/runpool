package consistency

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"golang.org/x/mod/modfile"
	"gopkg.in/yaml.v3"
)

// builderStage matches the Go builder a Dockerfile builds from. There is
// no Dockerfile parser in this module's graph and one line of a stable
// format does not justify adding one; go.mod and the workflows are
// parsed rather than matched, because both have parsers here already.
var builderStage = regexp.MustCompile(`(?m)^FROM (?:--platform=\S+ )?golang:([0-9][^-@ ]*)`)

// goClaim matches a document naming a complete Go patch version, in
// prose or inside a badge URL. A bare "Go 1.26" is a language version
// and not a claim about the toolchain, so it is not matched.
var goClaim = regexp.MustCompile(`(?:Go[- ])([0-9]+\.[0-9]+\.[0-9]+)`)

// TestEveryBuilderAndGateNamesTheGoThatGoModDeclares: an image tag, a
// workflow variable and a module directive are three ecosystems, and
// every updater sees one of them.
//
// They drift silently and the drift is invisible in a green build: the
// binary an image ships would be compiled by a toolchain no test ever
// ran, which is the one thing a reproducible build is supposed to rule
// out. The workflow half is the same argument one step earlier — a gate
// that resolves its own compiler proves something about a toolchain the
// release may never use.
//
// This replaces a shell check that read all three with sed. Dependabot
// is configured to propose only patch bumps for the builder, so this is
// the check behind that policy rather than a substitute for it.
func TestEveryBuilderAndGateNamesTheGoThatGoModDeclares(t *testing.T) {
	want := declaredGo(t)

	builders, err := filepath.Glob(repoPath("build", "*", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	if len(builders) == 0 {
		t.Fatal("no Dockerfile builds anything, so this proves nothing")
	}
	for _, dockerfile := range builders {
		body, err := os.ReadFile(dockerfile)
		if err != nil {
			t.Fatal(err)
		}
		found := builderStage.FindAllStringSubmatch(string(body), -1)
		if len(found) == 0 {
			t.Errorf("%s has no golang builder stage", rel(dockerfile))
			continue
		}
		for _, match := range found {
			if match[1] != want {
				t.Errorf("%s builds with golang:%s; go.mod declares %s",
					rel(dockerfile), match[1], want)
			}
		}
	}

	workflows, err := filepath.Glob(repoPath(".github", "workflows", "*.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(workflows) == 0 {
		t.Fatal("no workflow runs anything, so this proves nothing")
	}
	for _, workflow := range workflows {
		body, err := os.ReadFile(workflow)
		if err != nil {
			t.Fatal(err)
		}
		var document yaml.Node
		if err := yaml.Unmarshal(body, &document); err != nil {
			t.Errorf("parse %s: %v", rel(workflow), err)
			continue
		}
		// Every level, not the workflow's own: a job or a step may set
		// its own, and the one that disagrees is the drift this exists
		// to find.
		pinned := valuesOf(&document, "GOTOOLCHAIN")
		if len(pinned) == 0 {
			t.Errorf("%s pins no GOTOOLCHAIN, so its steps may resolve another compiler",
				rel(workflow))
			continue
		}
		for _, got := range pinned {
			if got != "go"+want {
				t.Errorf("%s pins GOTOOLCHAIN %s; go.mod declares %s", rel(workflow), got, want)
			}
		}
	}

	// The fourth ecosystem is prose, and it drifted furthest: the landing
	// page's own badge, the line beneath it naming the prerequisite, and
	// the contributing guide citing go.mod by name all said 1.26.6 while
	// go.mod said 1.26.7. A contributor installs what the page tells
	// them to and meets a toolchain error the page cannot explain.
	told := 0
	for _, file := range tracked(t, "*.md") {
		body, err := os.ReadFile(repoPath(file))
		if err != nil {
			t.Fatal(err)
		}
		for number, line := range strings.Split(string(body), "\n") {
			for _, match := range goClaim.FindAllStringSubmatch(line, -1) {
				told++
				if match[1] != want {
					t.Errorf("%s:%d names Go %s; go.mod declares %s",
						file, number+1, match[1], want)
				}
			}
		}
	}
	if told == 0 {
		t.Fatal("no document names a Go version, so this proves nothing")
	}

	// A `go-version:` scalar is a toolchain chosen by hand, wherever it
	// is: the workflows here all say `go-version-file: go.mod` and are
	// covered above, but the end-to-end fixture pins a version literally,
	// and it was the last 1.26.6 in the tree with nothing reading it.
	for _, file := range tracked(t, "*.yml", "*.yaml") {
		body, err := os.ReadFile(repoPath(file))
		if err != nil {
			t.Fatal(err)
		}
		var document yaml.Node
		if err := yaml.Unmarshal(body, &document); err != nil {
			continue // not every tracked YAML is a document this parses
		}
		for _, got := range valuesOf(&document, "go-version") {
			if got != want {
				t.Errorf("%s pins go-version %s; go.mod declares %s", file, got, want)
			}
		}
	}
}

// declaredGo is the version go.mod's own directive names, read with the
// parser the Go toolchain uses for it.
func declaredGo(t *testing.T) string {
	t.Helper()
	path := repoPath("go.mod")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := modfile.Parse(path, body, nil)
	if err != nil {
		t.Fatalf("parse go.mod: %v", err)
	}
	if parsed.Go == nil || parsed.Go.Version == "" {
		t.Fatal("go.mod declares no Go version")
	}
	return parsed.Go.Version
}

// valuesOf collects every scalar a mapping key holds anywhere in a
// document, at any depth.
func valuesOf(node *yaml.Node, key string) []string {
	var found []string
	if node.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(node.Content); i += 2 {
			if node.Content[i].Value == key && node.Content[i+1].Kind == yaml.ScalarNode {
				found = append(found, node.Content[i+1].Value)
			}
		}
	}
	for _, child := range node.Content {
		found = append(found, valuesOf(child, key)...)
	}
	return found
}

func rel(path string) string {
	return strings.TrimPrefix(filepath.ToSlash(path), "../../")
}
