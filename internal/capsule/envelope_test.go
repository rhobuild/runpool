package capsule

import (
	"strings"
	"testing"

	"github.com/rhobuild/runpool/internal/config"
)

// TestSplitEnvelopeIsConserving is the property the threat model rests
// on: a lease cannot spend more than its tier. The gateway is
// workload-driven work, so it comes out of the same envelope — the sum
// of the two shares must be exactly the tier, never the tier plus a
// container nobody budgeted for.
func TestSplitEnvelopeIsConserving(t *testing.T) {
	tier := config.Resources{
		Memory: 4 << 30,
		Swap:   1 << 30,
		CPU:    4_000_000_000,
		PIDs:   1024,
	}
	capsuleShare := SplitEnvelope(tier, true)
	gw := GatewayEnvelope()

	if got := capsuleShare.MemoryBytes + gw.MemoryBytes; got != int64(tier.Memory) {
		t.Errorf("memory: capsule %d + gateway %d = %d; want the tier's %d",
			capsuleShare.MemoryBytes, gw.MemoryBytes, got, int64(tier.Memory))
	}
	if got := capsuleShare.MemorySwapBytes + gw.MemorySwapBytes; got != int64(tier.Memory+tier.Swap) {
		t.Errorf("memory+swap total = %d; want %d", got, int64(tier.Memory+tier.Swap))
	}
	if got := capsuleShare.NanoCPUs + gw.NanoCPUs; got != int64(tier.CPU) {
		t.Errorf("cpu: %d + %d = %d; want %d", capsuleShare.NanoCPUs, gw.NanoCPUs, got, int64(tier.CPU))
	}
	if got := capsuleShare.PIDsLimit + gw.PIDsLimit; got != tier.PIDs {
		t.Errorf("pids: %d + %d = %d; want %d", capsuleShare.PIDsLimit, gw.PIDsLimit, got, tier.PIDs)
	}

	// Conservation alone holds for any reserve, zero included: the
	// identity (tier - K) + K == tier says nothing about K. And zero is
	// the one value Docker reads as a different word — Memory 0,
	// NanoCPUs 0 and PidsLimit 0 are unlimited, so a zeroed reserve turns
	// the gateway into an unbounded container outside any tier budget,
	// with this test green. Both shares are bounded, strictly.
	for name, pair := range map[string][2]int64{
		"memory": {gw.MemoryBytes, capsuleShare.MemoryBytes},
		"cpu":    {gw.NanoCPUs, capsuleShare.NanoCPUs},
		"pids":   {gw.PIDsLimit, capsuleShare.PIDsLimit},
	} {
		if pair[0] <= 0 {
			t.Errorf("the gateway's %s share is %d; zero is unlimited to the daemon, an unbounded "+
				"container outside any tier budget", name, pair[0])
		}
		if pair[1] <= 0 {
			t.Errorf("the capsule's %s share is %d; nothing can boot under it", name, pair[1])
		}
	}
}

// TestSplitEnvelopeWithoutGateway: the unsafe-open profile has no
// gateway, so the capsule takes the whole tier and nothing is withheld.
func TestSplitEnvelopeWithoutGateway(t *testing.T) {
	tier := config.Resources{Memory: 2 << 30, Swap: 512 << 20, CPU: 2e9, PIDs: 512}
	got := SplitEnvelope(tier, false)
	if got.MemoryBytes != int64(tier.Memory) || got.MemorySwapBytes != int64(tier.Memory+tier.Swap) ||
		got.NanoCPUs != int64(tier.CPU) || got.PIDsLimit != tier.PIDs {
		t.Errorf("without a gateway the capsule share = %+v; want the whole tier", got)
	}
}

// TestLeaseCgroupParentMatchesTheDriver: the daemon validates this
// string and refuses the wrong form, so the driver decides it. A
// systemd host wants a slice unit; a cgroupfs host wants a path.
func TestLeaseCgroupParentMatchesTheDriver(t *testing.T) {
	const lease = "abcdef0123456789"

	systemd := LeaseCgroupParent("systemd", lease)
	if !strings.HasSuffix(systemd, ".slice") {
		t.Errorf("systemd parent %q must be a slice unit", systemd)
	}
	if strings.HasPrefix(systemd, "/") {
		t.Errorf("systemd parent %q must not be a path", systemd)
	}

	// A dash is systemd's hierarchy separator, so an id carrying one
	// once produced `runpool-lease-x-.slice` — rejected by the daemon,
	// which failed the whole launch. Ids are sanitized, not trusted.
	for _, hostile := range []string{"contract-", "-", "a-b-c-", "with.dots", "sp ace", ""} {
		got := LeaseCgroupParent("systemd", hostile)
		if got == "" {
			continue // nothing usable in the id: no parent, the honest answer
		}
		name := strings.TrimSuffix(got, ".slice")
		if strings.HasSuffix(name, "-") {
			t.Errorf("lease %q produced %q, which systemd rejects for the trailing dash", hostile, got)
		}
		if strings.ContainsAny(name, ". /") {
			t.Errorf("lease %q produced %q, which is not a valid unit name", hostile, got)
		}
	}

	// A driver the code does not know means no parent at all: guessing
	// produces a container the daemon refuses to create.
	if got := LeaseCgroupParent("", lease); got != "" {
		t.Errorf("unknown driver produced parent %q; want none", got)
	}

	cgroupfs := LeaseCgroupParent("cgroupfs", lease)
	if !strings.HasPrefix(cgroupfs, "/") {
		t.Errorf("cgroupfs parent %q must be a path", cgroupfs)
	}
	if strings.HasSuffix(cgroupfs, ".slice") {
		t.Errorf("cgroupfs parent %q must not be a slice unit", cgroupfs)
	}

	// Two leases never share a parent: the aggregate is per lease, and
	// sharing one would let a lease read — or be charged for — another.
	if LeaseCgroupParent("systemd", "aaaaaaaaaaaa") == LeaseCgroupParent("systemd", "bbbbbbbbbbbb") {
		t.Error("two leases produced the same parent cgroup")
	}
	// The same lease always names the same parent, which is what puts
	// its capsule and its gateway in one place — they are created by
	// separate calls, so a non-deterministic name would silently give
	// them separate budgets.
	first := LeaseCgroupParent("systemd", lease)
	second := LeaseCgroupParent("systemd", lease)
	if first != second {
		t.Errorf("the parent cgroup is not deterministic: %q then %q", first, second)
	}
}

// TestKnownCgroupDriverGuardsTheEnvelope: the parent cgroup's form is
// written per driver, and a driver with no form produces no parent at
// all — which Docker reads as "no parent", putting the capsule and its
// gateway in separate budgets. The tier stops being their sum, silently,
// so the answer has to be reachable before a capsule is created.
func TestKnownCgroupDriverGuardsTheEnvelope(t *testing.T) {
	for driver, known := range map[string]bool{
		"systemd":  true,
		"cgroupfs": true,
		"":         false,
		"none":     false,
		"Systemd":  false, // the daemon reports it lowercase; anything else is not it
	} {
		if got := KnownCgroupDriver(driver); got != known {
			t.Errorf("KnownCgroupDriver(%q) = %v, want %v", driver, got, known)
		}
		// The guard and the generator have to agree, or the guard is
		// approving a driver that still yields no parent.
		parent := LeaseCgroupParent(driver, "lease-abc123")
		if known != (parent != "") {
			t.Errorf("driver %q: known=%v but parent=%q", driver, known, parent)
		}
	}
}
