package app

import (
	"errors"
	"testing"
	"time"

	"github.com/rhobuild/runpool/internal/cache"
	"github.com/rhobuild/runpool/internal/config"
	"github.com/rhobuild/runpool/internal/disk"
	"github.com/rhobuild/runpool/internal/engine"
	"github.com/rhobuild/runpool/internal/store"
)

// TestMeasureWeighsOnlyThisInstancesLanes: the managed figure is what the
// watermark passes work against, so anything counted into it that a
// collection cannot evict makes the monitor chase a number it cannot
// move. Only cache lanes are evictable, and a volume whose size the
// daemon could not compute counts as zero rather than as a guess.
func TestMeasureWeighsOnlyThisInstancesLanes(t *testing.T) {
	h := newHarness(t, 1)
	h.probe.free = engine.FilesystemFree{FreeBytes: 500, FreeInodes: 90}
	h.probe.usage = []engine.VolumeUsage{
		{Name: "lane-a", Size: 100, Role: cache.RoleCacheLane},
		{Name: "lane-b", Size: 40, Role: cache.RoleCacheLane},
		{Name: "workspace", Size: 9000, Role: "workspace"},
		{Name: "unmeasured", Size: -1, Role: cache.RoleCacheLane},
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
	h.srv.alloc.SetAssignedDemand(h.bind.key, 2)
	before := h.srv.alloc.Advertised(h.bind.key)
	if before == 0 {
		t.Fatal("a binding with demand and room must advertise before the emergency")
	}
	h.probe.free = engine.FilesystemFree{FreeBytes: 0, FreeInodes: 0}

	h.srv.disk.pass(t.Context())

	level := h.srv.currentPressure()
	if !level.AdmissionClosed() {
		t.Fatalf("level on a full filesystem = %s; admission must be closed", level)
	}
	// The gate itself, not only the level: the pass owns its own Hold
	// call, separate from resume's, and a pass that recorded the level
	// without moving the gate kept the broker fed on a full filesystem.
	if got := h.srv.alloc.Advertised(h.bind.key); got != 0 {
		t.Errorf("advertised %d under the emergency; want 0", got)
	}
	if h.srv.alloc.TryReserve(h.bind.key) {
		t.Error("a reserve succeeded under the emergency; the capsule would start on the full filesystem")
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

	// And the way back. No test drove recovery through the pass itself:
	// the one that covered the closing direction lifted the hold by hand,
	// so a host that recovered kept advertising nothing -- silently out
	// of service while status reported normal pressure.
	h.probe.free = engine.FilesystemFree{FreeBytes: 1 << 40, FreeInodes: 1 << 20}
	h.srv.disk.pass(t.Context())
	if got := h.srv.currentPressure(); got.AdmissionClosed() {
		t.Fatalf("level after recovery = %s; admission must reopen", got)
	}
	if got := h.srv.alloc.Advertised(h.bind.key); got != before {
		t.Errorf("advertised %d after recovery; want %d back", got, before)
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
