package engine

import (
	"encoding/json"
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
