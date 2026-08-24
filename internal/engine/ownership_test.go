package engine

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"slices"
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
	pinned := map[Role]string{
		RoleCapsule:        "capsule",
		RoleGateway:        "gateway",
		RoleCapsuleNetwork: "capsule-net",
		RoleDindData:       "dind-data",
		RoleUplink:         "uplink",
		RoleCacheLane:      "cache-lane",
		RoleProbe:          "probe",
		RolePreflightProbe: "preflight-probe",
	}
	for role, want := range pinned {
		if string(role) != want {
			t.Errorf("role renders %q; the label says %q, and an instance upgraded "+
				"mid-flight would stop recognising what the previous build owns", role, want)
		}
	}

	// Totality: a role added to the vocabulary and not here is one whose
	// sweep class nobody decided.
	seen := map[string]bool{}
	for _, want := range pinned {
		if seen[want] {
			t.Errorf("two roles share the wire value %q", want)
		}
		seen[want] = true
	}
	// Against the source, not against itself: a count written here would
	// be a number this test also chose, and a ninth constant would pass.
	declared := roleConstants(t)
	if len(declared) != len(pinned) {
		t.Errorf("engine.go declares %d roles and this pin holds %d: %v\n"+
			"decide the new role's sweep class -- does it carry a lease? -- and add it here",
			len(declared), len(pinned), declared)
	}
	for _, name := range declared {
		if !slices.ContainsFunc(slices.Collect(maps.Keys(pinned)), func(r Role) bool {
			return roleName(r) == name
		}) {
			t.Errorf("the constant %s is not pinned to a wire value", name)
		}
	}
}

// roleConstants reads this package's own source for every constant
// declared with type Role.
func roleConstants(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "engine.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			v, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			if id, ok := v.Type.(*ast.Ident); !ok || id.Name != "Role" {
				continue
			}
			for _, n := range v.Names {
				out = append(out, n.Name)
			}
		}
	}
	return out
}

// roleName maps a value back to the constant that names it, which is the
// half a source parse cannot do.
func roleName(r Role) string {
	switch r {
	case RoleCapsule:
		return "RoleCapsule"
	case RoleGateway:
		return "RoleGateway"
	case RoleCapsuleNetwork:
		return "RoleCapsuleNetwork"
	case RoleDindData:
		return "RoleDindData"
	case RoleUplink:
		return "RoleUplink"
	case RoleCacheLane:
		return "RoleCacheLane"
	case RoleProbe:
		return "RoleProbe"
	case RolePreflightProbe:
		return "RolePreflightProbe"
	}
	return ""
}
