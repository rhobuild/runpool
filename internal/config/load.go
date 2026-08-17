package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	"gopkg.in/yaml.v3"
)

// Load builds the effective configuration from the process environment:
// file mode when RUNPOOL_CONFIG_FILE is set, Quick Start translation
// otherwise. The result is defaulted and validated.
func Load(environ func(string) string) (*Config, error) {
	if path := environ(EnvConfigFile); path != "" {
		for _, name := range quickStartTargetVars {
			if environ(name) != "" {
				return nil, fmt.Errorf("%s and %s are mutually exclusive: with a configuration file, target, tier and capacity settings come from the file", EnvConfigFile, name)
			}
		}
		return LoadFile(path)
	}
	c, err := FromEnvironment(environ)
	if err != nil {
		return nil, err
	}
	return finish(c)
}

// Input limits for a configuration file. A configuration is written by
// a person and read once at startup; none of these bounds constrains a
// real one, and each bounds what a hostile or broken file can make the
// parser do before any field is even looked at.
const (
	// MaxConfigBytes is generous for a file whose largest realistic
	// form is a few dozen targets.
	MaxConfigBytes = 1 << 20
	// maxConfigDepth bounds nesting; the deepest legitimate path is
	// about six levels (targets, then tiers, then their fields).
	maxConfigDepth = 32
	// maxConfigNodes bounds the total node count, which is the quantity
	// an expansion attack inflates.
	maxConfigNodes = 20000
)

// LoadFile reads, defaults and validates an advanced configuration
// file. Unknown fields are errors: a typo must fail, not silently
// configure nothing.
//
// The document is checked before it is decoded. YAML can describe
// structures that cost far more to build than to write — aliases
// expanding into each other, unbounded nesting — so size, depth, node
// count and alias use are bounded first, and only a document within
// those bounds is decoded into the configuration.
func LoadFile(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// Reading one byte past the limit is what distinguishes a file at
	// the limit from one truncated at it.
	raw, err := io.ReadAll(io.LimitReader(f, MaxConfigBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(raw) > MaxConfigBytes {
		return nil, fmt.Errorf("%s: configuration exceeds %d bytes", path, MaxConfigBytes)
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := checkDocument(&doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := dec.Decode(new(struct{})); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: expected a single YAML document", path)
	}
	return finish(&c)
}

// checkDocument walks the parsed tree and enforces the input limits.
// Anchors and aliases are refused outright rather than bounded: a
// configuration that reuses a node is harder to read than one that
// repeats it, so refusing them removes expansion attacks entirely
// instead of pricing them.
func checkDocument(doc *yaml.Node) error {
	nodes := 0
	var walk func(n *yaml.Node, depth int) error
	walk = func(n *yaml.Node, depth int) error {
		if n == nil {
			return nil
		}
		if depth > maxConfigDepth {
			return fmt.Errorf("configuration nests deeper than %d levels", maxConfigDepth)
		}
		nodes++
		if nodes > maxConfigNodes {
			return fmt.Errorf("configuration has more than %d nodes", maxConfigNodes)
		}
		if n.Kind == yaml.AliasNode {
			return fmt.Errorf("line %d: YAML aliases are not accepted; write the value out", n.Line)
		}
		if n.Anchor != "" {
			return fmt.Errorf("line %d: YAML anchors are not accepted; write the value out", n.Line)
		}
		for _, child := range n.Content {
			if err := walk(child, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	return walk(doc, 0)
}

func finish(c *Config) (*Config, error) {
	ApplyDefaults(c)
	if err := Validate(c); err != nil {
		return nil, err
	}
	return c, nil
}
