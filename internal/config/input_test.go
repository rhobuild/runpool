package config

import (
	"strings"
	"testing"
)

// The configuration file is parsed before it is trusted, so these
// bounds are checked at the parser, not at the fields. Each case is a
// document that is cheap to write and expensive to build.

func loadText(t *testing.T, content string) error {
	t.Helper()
	path := t.TempDir() + "/config.yaml"
	if err := writeFile(path, content); err != nil {
		t.Fatal(err)
	}
	_, err := LoadFile(path)
	return err
}

const configHeader = "apiVersion: runpool.rhobuild.com/v1\nkind: RunpoolConfig\n"

// TestRejectsOversizedFile: the reader stops at the limit rather than
// pulling an arbitrarily large file into memory.
func TestRejectsOversizedFile(t *testing.T) {
	padding := strings.Repeat("# padding to exceed the input limit\n", (MaxConfigBytes/36)+64)
	err := loadText(t, configHeader+padding)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("oversized configuration = %v; want a size error", err)
	}
}

// TestRejectsAliases is the billion-laughs family: an alias graph is
// small on disk and enormous in memory. Refusing aliases outright
// removes the class instead of pricing it.
func TestRejectsAliases(t *testing.T) {
	bomb := configHeader + `
a: &a ["x","x","x","x","x","x","x","x","x"]
b: &b [*a,*a,*a,*a,*a,*a,*a,*a,*a]
c: &c [*b,*b,*b,*b,*b,*b,*b,*b,*b]
targets: [*c,*c,*c,*c,*c,*c,*c,*c,*c]
`
	err := loadText(t, bomb)
	if err == nil {
		t.Fatal("an alias expansion document was accepted")
	}
	if !strings.Contains(err.Error(), "anchor") && !strings.Contains(err.Error(), "alias") {
		t.Errorf("error = %v; want it to name anchors or aliases", err)
	}
}

// TestRejectsDeepNesting: depth is what turns a recursive walk into a
// stack problem, so it is bounded before the tree is walked for real.
func TestRejectsDeepNesting(t *testing.T) {
	err := loadText(t, configHeader+"targets: "+strings.Repeat("[", 200)+strings.Repeat("]", 200)+"\n")
	if err == nil || !strings.Contains(err.Error(), "deeper") {
		t.Errorf("deeply nested configuration = %v; want a depth error", err)
	}
}

// TestRejectsDuplicateKeys: two values for one key means the file says
// two different things, and silently taking the last is how a
// configuration ends up meaning something nobody wrote.
func TestRejectsDuplicateKeys(t *testing.T) {
	err := loadText(t, configHeader+"instance:\n  name: first\n  name: second\n")
	if err == nil {
		t.Fatal("a document with a duplicate key was accepted")
	}
}

// TestRejectsMultipleDocuments: a second document would be silently
// ignored, and an operator who wrote one expects it to take effect.
func TestRejectsMultipleDocuments(t *testing.T) {
	err := loadText(t, configHeader+"---\n"+configHeader)
	if err == nil || !strings.Contains(err.Error(), "single YAML document") {
		t.Errorf("multi-document configuration = %v; want a single-document error", err)
	}
}

// TestAcceptsARealConfiguration keeps the limits honest: they must not
// reject the file the project ships as its own example.
func TestAcceptsARealConfiguration(t *testing.T) {
	if _, err := LoadFile("testdata/example.yaml"); err != nil {
		t.Fatalf("the shipped example configuration was rejected: %v", err)
	}
}
