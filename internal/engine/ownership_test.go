package engine

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// TestTheLabelsAreExactlyThese: the values are a compatibility surface
// between releases, and nothing else pins them.
//
// A controller sweeps the objects an older controller stamped. It finds
// them by label, so a key that is renamed or a value that is spelled
// differently is a sweep that stops finding a whole release's objects —
// which is a capacity leak and an operator deleting containers by hand.
// Nothing about that fails at compile time, and no live suite catches it
// either: both sides of a contract run would use the new spelling and
// agree.
//
// So the exact document is written out here. Changing it is a decision,
// and this is where it has to be taken.
func TestTheLabelsAreExactlyThese(t *testing.T) {
	full := Ownership{
		Instance: "instance-1",
		Lease:    "lease-1",
		Kind:     "container",
		Role:     "capsule",
		Attempt:  "attempt-1",
		Target:   "target-1",
		Tier:     "tier-1",
	}
	const want = `{"io.runpool.attempt":"attempt-1","io.runpool.instance":"instance-1",` +
		`"io.runpool.kind":"container","io.runpool.lease":"lease-1","io.runpool.managed":"true",` +
		`"io.runpool.role":"capsule","io.runpool.target":"target-1","io.runpool.tier":"tier-1"}`
	if got := marshal(t, full.Labels()); got != want {
		t.Errorf("the labels are\n  %s\nand a release that swept the previous one's objects expects\n  %s",
			got, want)
	}

	// Instance infrastructure carries no lease, and that absence is what
	// InstanceInfrastructure reads to leave it alone. An unset field
	// written as an empty label would still be absent to a reader, but
	// it would put a key on the object that says nothing.
	uplink := Ownership{Instance: "instance-1", Role: "uplink"}
	const wantUplink = `{"io.runpool.instance":"instance-1","io.runpool.managed":"true",` +
		`"io.runpool.role":"uplink"}`
	if got := marshal(t, uplink.Labels()); got != wantUplink {
		t.Errorf("infrastructure labels are\n  %s\nwant\n  %s", got, wantUplink)
	}
}

// marshal renders the labels in key order, which is what makes the
// comparison exact rather than approximately equal.
func marshal(t *testing.T, labels map[string]string) string {
	t.Helper()
	encoded, err := json.Marshal(labels)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// TestEveryRoleIsPinnedToItsWireValue is this vocabulary's totality list
// and its compatibility pin at once, and both halves need the literals.
//
// The values are stamped into labels that outlive the process that wrote
// them: an instance upgraded mid-flight has to recognise what the
// previous build owns, so a value edited here is a sweep that stops
// seeing its own objects. Spelled through the constants this would pin
// nothing -- edit RoleUplink and a constant-using assertion drifts with
// it and stays green. The literal is the only thing that fails.
//
// There is no production Roles slice, deliberately: the sweep refuses
// role enumeration (see HelperInFlight and InstanceInfrastructure), so a
// list nothing branches on would be a second place to keep in step. What
// adding a role obliges is a decision here and there -- whether it
// carries a lease, and therefore whether an object wearing it is
// infrastructure or an orphan.
func TestEveryRoleIsPinnedToItsWireValue(t *testing.T) {
	pinned := map[string]string{
		"RoleCapsule":        "capsule",
		"RoleGateway":        "gateway",
		"RoleCapsuleNetwork": "capsule-net",
		"RoleDindData":       "dind-data",
		"RoleUplink":         "uplink",
		"RoleCacheLane":      "cache-lane",
		"RoleProbe":          "probe",
		"RolePreflightProbe": "preflight-probe",
	}
	declared := roleConstants(t)

	for name, value := range declared {
		want, ok := pinned[name]
		if !ok {
			t.Errorf("%s is declared and not pinned. Decide its sweep class -- does it "+
				"carry a lease? -- and add it here with its wire value", name)
			continue
		}
		if value != want {
			t.Errorf("%s renders %q; the label says %q, and an instance upgraded mid-flight "+
				"would stop recognising what the previous build owns", name, value, want)
		}
	}
	for name := range pinned {
		if _, ok := declared[name]; !ok {
			t.Errorf("%s is pinned here and no longer declared", name)
		}
	}
	seen := map[string]string{}
	for name, value := range declared {
		if other, dup := seen[value]; dup {
			t.Errorf("%s and %s share the wire value %q", other, name, value)
		}
		seen[value] = name
	}
}

// roleConstants reads this package's own source for every constant
// declared with type Role, and returns each one's literal value. Taking
// the value from the source rather than mapping it back by hand is what
// keeps this to two lists -- the declaration and the pin above -- rather
// than three, where the third would be the one nobody remembers.
//
// It walks every file in the package, not one: a constant added in a new
// file would otherwise escape the count in silence.
func roleConstants(t *testing.T) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				v, ok := spec.(*ast.ValueSpec)
				if !ok || len(v.Names) != len(v.Values) || !declaresRole(v) {
					continue
				}
				for i, n := range v.Names {
					lit, ok := literalOf(v.Values[i])
					if !ok {
						t.Errorf("%s is a Role constant this parse cannot read a value from; "+
							"spell it as a plain string literal so the pin can see it", n.Name)
						continue
					}
					out[n.Name] = lit
				}
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("no Role constants found, so this proves nothing")
	}
	return out
}

// declaresRole covers both spellings a Role constant can take: the typed
// form `RoleX Role = "x"` and the conversion form `RoleX = Role("x")`.
// Counting only the first would let the second escape.
func declaresRole(v *ast.ValueSpec) bool {
	if id, ok := v.Type.(*ast.Ident); ok && id.Name == "Role" {
		return true
	}
	for _, val := range v.Values {
		if call, ok := val.(*ast.CallExpr); ok {
			if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "Role" {
				return true
			}
		}
	}
	return false
}

func literalOf(expr ast.Expr) (string, bool) {
	if call, ok := expr.(*ast.CallExpr); ok && len(call.Args) == 1 {
		expr = call.Args[0]
	}
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	return strings.Trim(lit.Value, `"`), true
}
