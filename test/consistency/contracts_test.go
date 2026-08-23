package consistency

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
)

var shellAssignment = regexp.MustCompile(`(?m)^fixture='([^']+)'`)

// TestTheHarnessClearsTheFixtureTheSuitePulls: the pull path only exists
// while the image is absent.
//
// TestPullOnMissingImage proves that a container creation on an image the
// daemon does not have pulls it first. After one run the daemon has it
// cached, so the harness removes it before the suite starts and proves
// the removal. Remove something else — a stale tag, a digest that moved —
// and the removal succeeds, the fixture stays cached, and the test passes
// having exercised nothing.
//
// The two live in different languages and neither can see the other.
func TestTheHarnessClearsTheFixtureTheSuitePulls(t *testing.T) {
	const harnessPath = "test/contract/docker/remote-harness.sh"

	harness, err := os.ReadFile(repoPath(filepath.FromSlash(harnessPath)))
	if err != nil {
		t.Fatal(err)
	}
	match := shellAssignment.FindSubmatch(harness)
	if match == nil {
		t.Fatalf("%s assigns no fixture, so it clears nothing before the suite runs", harnessPath)
	}
	cleared := string(match[1])

	pulled := stringConst(t, filepath.Join("test", "contract", "docker", "contract_test.go"), "missingImage")
	if cleared != pulled {
		t.Errorf("%s clears %s; the suite pulls %s, which stays cached and turns "+
			"TestPullOnMissingImage into a no-op", harnessPath, cleared, pulled)
	}
}

// stringConst is the value of a named string constant, read from the
// declaration rather than matched in the text: a constant is a value the
// compiler already knows, and the file is Go.
func stringConst(t *testing.T, file, name string) string {
	t.Helper()
	path := repoPath(filepath.FromSlash(file))
	parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	for _, decl := range parsed.Decls {
		general, ok := decl.(*ast.GenDecl)
		if !ok || general.Tok != token.CONST {
			continue
		}
		for _, spec := range general.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok || len(value.Names) != 1 || len(value.Values) != 1 {
				continue
			}
			if value.Names[0].Name != name {
				continue
			}
			literal, ok := value.Values[0].(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				t.Fatalf("%s in %s is not a string literal", name, file)
			}
			unquoted, err := strconv.Unquote(literal.Value)
			if err != nil {
				t.Fatal(err)
			}
			return unquoted
		}
	}
	t.Fatalf("%s declares no constant %s, so this proves nothing", file, name)
	return ""
}

// readRepoJSON decodes one of the repository's own JSON documents.
func readRepoJSON(t *testing.T, file string, into any) error {
	t.Helper()
	body, err := os.ReadFile(repoPath(filepath.FromSlash(file)))
	if err != nil {
		return err
	}
	return json.Unmarshal(body, into)
}
