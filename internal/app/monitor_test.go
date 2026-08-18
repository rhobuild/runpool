package app

import (
	"errors"
	"testing"
	"time"

	"slices"

	"github.com/rhobuild/runpool/internal/cache"
	"github.com/rhobuild/runpool/internal/capsule"
	"github.com/rhobuild/runpool/internal/config"
	"github.com/rhobuild/runpool/internal/disk"
	"github.com/rhobuild/runpool/internal/platform/docker"
	"github.com/rhobuild/runpool/internal/store"
)

// TestMeasureWeighsOnlyThisInstancesLanes: the managed figure is what the
// watermark passes work against, so anything counted into it that a
// collection cannot evict makes the monitor chase a number it cannot
// move. Only cache lanes are evictable, and a volume whose size the
// daemon could not compute counts as zero rather than as a guess.
func TestMeasureWeighsOnlyThisInstancesLanes(t *testing.T) {
	h := newHarness(t, 1)
	h.probe.free = docker.FilesystemFree{FreeBytes: 500, FreeInodes: 90}
	h.probe.usage = []docker.VolumeUsage{
		{Name: "lane-a", Size: 100, Labels: map[string]string{docker.LabelRole: cache.RoleCacheLane}},
		{Name: "lane-b", Size: 40, Labels: map[string]string{docker.LabelRole: cache.RoleCacheLane}},
		{Name: "workspace", Size: 9000, Labels: map[string]string{docker.LabelRole: "workspace"}},
		{Name: "unmeasured", Size: -1, Labels: map[string]string{docker.LabelRole: cache.RoleCacheLane}},
	}

	facts, err := h.srv.disk.measure(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if facts.ManagedBytes != 140 {
		t.Errorf("managed = %d; want only the two measured lanes, 140", facts.ManagedBytes)
	}
	if facts.FreeBytes != 500 || facts.FreeInodes != 90 {
		t.Errorf("facts = %+v; want the probe's own figures", facts)
	}
}

// TestAFailedMeasurementKeepsTheLevelInForce. Losing the probe must not
// reopen an admission an emergency closed: the daemon being unreachable
// is not evidence that the disk drained.
func TestAFailedMeasurementKeepsTheLevelInForce(t *testing.T) {
	h := newHarness(t, 1)
	h.srv.disk.level.Store(int32(disk.HardEmergency))
	h.probe.freeErr = errors.New("daemon unreachable")

	h.srv.disk.pass(t.Context())

	if got := h.srv.currentPressure(); got != disk.HardEmergency {
		t.Errorf("level after a failed pass = %s; want the one already in force", got)
	}
}

// TestPassClosesAdmissionAndPersistsIt drives the whole verdict on a
// filesystem with nothing left — the state a real daemon cannot be asked
// for. Three things have to happen together, and each covers a different
// failure: the credit pool closes so the broker is not handed work that
// will wait, the level is persisted so a restart resumes into it rather
// than admitting, and the change is audited.
func TestPassClosesAdmissionAndPersistsIt(t *testing.T) {
	h := newHarness(t, 1)
	h.probe.free = docker.FilesystemFree{FreeBytes: 0, FreeInodes: 0}

	h.srv.disk.pass(t.Context())

	level := h.srv.currentPressure()
	if !level.AdmissionClosed() {
		t.Fatalf("level on a full filesystem = %s; admission must be closed", level)
	}
	h.inStore(func(tx *store.Tx) error {
		p, err := tx.Pressure()
		if err != nil {
			return err
		}
		if p == nil || p.Level != level.String() {
			t.Errorf("persisted pressure = %+v; want %s, or a restart admits into an emergency", p, level)
		}
		return nil
	})

	// And the level survives a fresh monitor over the same store.
	resumed := newDiskMonitor(defaultedConfig(t), h.srv.log, h.store, h.probe,
		h.srv.cache, h.srv.alloc, "probe-image")
	if err := resumed.resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := resumed.current(); got != level {
		t.Errorf("resumed level = %s; want %s", got, level)
	}
}

// TestDiskPolicyFromReadsTheConfiguredThresholds: the policy is captured
// once at start, so a field read from the wrong place is a threshold that
// silently never applies.
func TestDiskPolicyFromReadsTheConfiguredThresholds(t *testing.T) {
	cfg := defaultedConfig(t)
	cfg.Cache.Global.MaxManagedBytes = 1 << 30
	cfg.Cache.Global.HighWatermarkPercent = 85
	cfg.Cache.Global.LowWatermarkPercent = 60
	cfg.Host.Reserve.FreeDisk = 1 << 20

	p := diskPolicyFrom(cfg)
	if p.thresholds.MaxManagedBytes != 1<<30 || p.thresholds.HighPct != 85 || p.thresholds.LowPct != 60 {
		t.Errorf("thresholds = %+v; want the configured watermarks", p.thresholds)
	}
	if p.thresholds.ReserveFreeBytes != 1<<20 {
		t.Errorf("reserve = %d; the host reserve is what the emergency floor is raised to",
			p.thresholds.ReserveFreeBytes)
	}
	if p.ttl != time.Duration(cfg.Cache.Defaults.UnusedTTL) {
		t.Errorf("ttl = %s; want the configured lane TTL", p.ttl)
	}
}

func defaultedConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := &config.Config{}
	config.ApplyDefaults(cfg)
	return cfg
}

// TestCollectGarbageRunsWhatTheLevelObliges: high pressure works the
// managed total down to the low watermark, a soft emergency takes every
// free lane — asserted against real lanes, because with an empty
// fixture both levels plan nothing and a pass that never forwarded the
// level would look identical. A fresh free lane surviving High and
// falling to SoftEmergency is the observable difference between the two
// postures.
func TestCollectGarbageRunsWhatTheLevelObliges(t *testing.T) {
	h := newHarness(t, 1)
	h.srv.disk.policy = diskPolicy{
		ttl:        30 * 24 * time.Hour,
		thresholds: disk.Thresholds{MaxManagedBytes: 1 << 40, LowPct: 60},
	}
	// One fresh, free lane: within TTL, within the managed budget.
	loc, ok, err := h.srv.cache.Acquire(t.Context(), "https://github.com/acme/app", "gen", "lease-gc", 4)
	if err != nil || !ok {
		t.Fatalf("acquire: ok=%v, %v", ok, err)
	}
	if err := h.store.Tx(t.Context(), func(tx *store.Tx) error {
		return tx.ReleaseCacheLane("lease-gc")
	}); err != nil {
		t.Fatal(err)
	}
	lanes := func() int {
		var n int
		if err := h.store.Tx(t.Context(), func(tx *store.Tx) error {
			all, err := tx.CacheLanes()
			n = len(all)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if lanes() != 1 {
		t.Fatalf("fixture holds %d lanes; want 1", lanes())
	}

	// High pressure: the pass runs, and a fresh lane under the watermark
	// is not its business.
	h.srv.disk.collectGarbage(t.Context(), disk.High)
	if lanes() != 1 {
		t.Fatal("high pressure evicted a fresh lane under the low watermark")
	}

	// Soft emergency: every free lane goes, TTL and watermark
	// notwithstanding — which is only observable if the level actually
	// reaches the planner as AllFree.
	h.srv.disk.collectGarbage(t.Context(), disk.SoftEmergency)
	if lanes() != 0 {
		t.Fatalf("a soft emergency left %d free lane(s); AllFree did not reach the planner", lanes())
	}
	_ = loc
}

// TestRediscoverClosesEveryGatewayWhenDiscoveryFails. The policy in force
// is not a safe fallback, only an older one: a network that appeared
// since it was computed is reachable and there is no way to tell. So a
// discovery that cannot be trusted closes every gateway rather than
// relaying under a set that cannot be shown to be current.
func TestRediscoverClosesEveryGatewayWhenDiscoveryFails(t *testing.T) {
	d := &fakeSandboxDaemon{
		uplinkID:     "up-1",
		uplinkSubnet: "172.30.0.0/24",
		probeOut:     "", // saw nothing: the deny set cannot be trusted
		containers: []docker.OwnedContainer{
			{ID: "gw-1", Role: capsule.RoleGateway, Running: true},
			{ID: "gw-2", Role: capsule.RoleGateway, Running: true},
		},
	}
	n := newTestSandbox(t, d, &capsule.Sandbox{UplinkNetworkID: "up-1"})

	n.rediscover(t.Context())

	if !slices.Equal(d.removed, []string{"gw-1", "gw-2"}) {
		t.Errorf("removed %v; a failed rediscovery has to close every gateway", d.removed)
	}
}
