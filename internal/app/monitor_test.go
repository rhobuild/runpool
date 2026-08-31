package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/rhobuild/runpool/internal/config"
	"github.com/rhobuild/runpool/internal/disk"
	"github.com/rhobuild/runpool/internal/engine"
	"github.com/rhobuild/runpool/internal/store"
)

type recordingAdmissionGate struct{ held bool }

func (g *recordingAdmissionGate) Hold(held bool) { g.held = held }

func newIsolatedDiskMonitor(t *testing.T, probe *fakeProbe) (*diskMonitor, *store.Store, *recordingAdmissionGate) {
	t.Helper()
	st, err := store.Open(t.TempDir(), store.DefaultRetryBudget)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	gate := &recordingAdmissionGate{}
	monitor := newDiskMonitor(defaultedConfig(t),
		slog.New(slog.NewTextHandler(io.Discard, nil)), st, probe, nil, gate, "probe-image")
	return monitor, st, gate
}

// TestANewMonitorClosesAdmissionUntilItMeasures proves the startup
// default at both boundaries delivery uses: the verdict is unknown and
// the allocator cannot announce demand before initialization.
func TestANewMonitorClosesAdmissionUntilItMeasures(t *testing.T) {
	h := newHarness(t, 1)
	h.srv.alloc.SetAssignedDemand(h.bind.key, 1)

	fresh := newDiskMonitor(defaultedConfig(t), h.srv.log, h.store, h.probe,
		h.srv.executor.cache, h.srv.alloc, "probe-image")
	if got := fresh.current(); got != disk.Unknown {
		t.Fatalf("new monitor level = %s; want unknown", got)
	}
	if got := h.srv.alloc.Advertised(h.bind.key); got != 0 {
		t.Fatalf("new monitor advertised %d before measuring; want 0", got)
	}
}

func TestFirstMeasurementFailureStaysUnknownAndPersistsIt(t *testing.T) {
	probe := &fakeProbe{freeErr: errors.New("daemon unavailable")}
	monitor, st, gate := newIsolatedDiskMonitor(t, probe)

	if err := monitor.initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := monitor.current(); got != disk.Unknown || !gate.held {
		t.Fatalf("after failed initialization level=%s held=%v; want unknown and held", got, gate.held)
	}
	if err := st.Tx(t.Context(), func(tx *store.Tx) error {
		p, err := tx.Pressure()
		if err != nil {
			return err
		}
		if p == nil || p.Level != disk.Unknown.String() || p.FreeBytes != -1 ||
			p.FreeInodes != -1 || p.ManagedBytes != -1 {
			t.Errorf("persisted unavailable verdict = %+v", p)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPersistedNormalDoesNotOpenAfterAProbeFailure(t *testing.T) {
	probe := &fakeProbe{freeErr: errors.New("daemon unavailable")}
	monitor, st, gate := newIsolatedDiskMonitor(t, probe)
	if err := st.Tx(t.Context(), func(tx *store.Tx) error {
		return tx.SetPressure(store.PressureVerdict{Level: disk.Normal.String(), FreeBytes: 1 << 40})
	}); err != nil {
		t.Fatal(err)
	}

	if err := monitor.initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := monitor.current(); got != disk.Unknown || !gate.held {
		t.Fatalf("stale normal plus failed probe produced level=%s held=%v; want unknown and held", got, gate.held)
	}
}

func TestSuccessfulInitializationPersistsBeforeOpening(t *testing.T) {
	probe := &fakeProbe{free: engine.FilesystemFree{FreeBytes: 1 << 40, FreeInodes: 1 << 20}}
	monitor, st, gate := newIsolatedDiskMonitor(t, probe)

	if err := monitor.initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := monitor.current(); got != disk.Normal || gate.held {
		t.Fatalf("successful initialization level=%s held=%v; want normal and open", got, gate.held)
	}
	if err := st.Tx(t.Context(), func(tx *store.Tx) error {
		p, err := tx.Pressure()
		if err != nil {
			return err
		}
		if p == nil || p.Level != disk.Normal.String() {
			t.Errorf("persisted verdict = %+v; admission opened without durable normal", p)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestMeasureWeighsOnlyThisInstancesLanes: the managed figure is what the
// watermark passes work against, so anything counted into it that a
// collection cannot evict makes the monitor chase a number it cannot
// move. Only cache lanes are evictable, and a volume whose size the
// daemon could not compute counts as zero rather than as a guess.
func TestMeasureWeighsOnlyThisInstancesLanes(t *testing.T) {
	h := newHarness(t, 1)
	h.probe.free = engine.FilesystemFree{FreeBytes: 500, FreeInodes: 90}
	h.probe.usage = []engine.VolumeUsage{
		{Name: "lane-a", Size: 100, Role: engine.RoleCacheLane},
		{Name: "lane-b", Size: 40, Role: engine.RoleCacheLane},
		{Name: "workspace", Size: 9000, Role: "workspace"},
		{Name: "unmeasured", Size: -1, Role: engine.RoleCacheLane},
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

// TestAFailedMeasurementKeepsAnEmergencyInForce. Losing the probe must
// not replace measured pressure with a weaker unknown verdict or reopen
// the admission that pressure closed.
func TestAFailedMeasurementKeepsAnEmergencyInForce(t *testing.T) {
	h := newHarness(t, 1)
	h.srv.alloc.SetAssignedDemand(h.bind.key, 1)
	h.srv.disk.apply(disk.HardEmergency)
	h.probe.freeErr = errors.New("daemon unreachable")

	h.srv.disk.pass(t.Context())

	if got := h.srv.currentPressure(); got != disk.HardEmergency {
		t.Errorf("level after a failed pass = %s; want the one already in force", got)
	}
	if got := h.srv.alloc.Advertised(h.bind.key); got != 0 {
		t.Errorf("advertised %d after the emergency probe failed; want 0", got)
	}
}

// TestClosingDoesNotDependOnPersistence checks the safety ordering: a
// full disk closes admission even when the store cannot record that fact.
func TestClosingDoesNotDependOnPersistence(t *testing.T) {
	h := newHarness(t, 1)
	h.srv.alloc.SetAssignedDemand(h.bind.key, 1)
	h.probe.free = engine.FilesystemFree{FreeBytes: 0, FreeInodes: 0}
	h.srv.disk.persist = func(context.Context, disk.Level, disk.Level, disk.Facts, string) error {
		return errors.New("store unavailable")
	}

	h.srv.disk.pass(t.Context())

	if got := h.srv.currentPressure(); got != disk.HardEmergency {
		t.Fatalf("level after a full-disk verdict whose write failed = %s; want hard_emergency", got)
	}
	if got := h.srv.alloc.Advertised(h.bind.key); got != 0 {
		t.Fatalf("advertised %d after the closing write failed; want 0", got)
	}
}

// TestOpeningDependsOnPersistence is the converse ordering: a measured
// recovery cannot reopen admission unless the recovery is durable.
func TestOpeningDependsOnPersistence(t *testing.T) {
	h := newHarness(t, 1)
	h.srv.alloc.SetAssignedDemand(h.bind.key, 1)
	h.srv.disk.apply(disk.HardEmergency)
	h.probe.free = engine.FilesystemFree{FreeBytes: 1 << 40, FreeInodes: 1 << 20}
	h.srv.disk.persist = func(context.Context, disk.Level, disk.Level, disk.Facts, string) error {
		return errors.New("store unavailable")
	}

	h.srv.disk.pass(t.Context())

	if got := h.srv.currentPressure(); got != disk.HardEmergency {
		t.Fatalf("level after a recovery whose write failed = %s; want hard_emergency", got)
	}
	if got := h.srv.alloc.Advertised(h.bind.key); got != 0 {
		t.Fatalf("advertised %d after the recovery write failed; want 0", got)
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
		h.srv.executor.cache, h.srv.alloc, "probe-image")
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
	loc, ok, err := h.srv.executor.cache.Acquire(t.Context(), "https://github.com/acme/app", "gen", "lease-gc", 4)
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

// TestASandboxFailureIsRecordedWhileTheControllerIsStopping is the
// delivery half of the egress report: the pass concludes, and something
// has to write that down where a reader in another process can find it.
//
// The context is cancelled first, deliberately. A rediscovery can fail
// and a shutdown begin before the write runs, and the context the loop
// was given is the one that has just been cancelled — so a write on it
// would fail exactly when it matters most. The gateways stay closed
// across a restart and the next pass is five minutes away, which is
// five minutes of an operator being told nothing.
func TestASandboxFailureIsRecordedWhileTheControllerIsStopping(t *testing.T) {
	h := newHarness(t, 1)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	h.srv.recordSandboxPass(ctx, errors.New("host discovery: the probe could not run"))

	var got *store.SandboxPass
	if err := h.srv.store.Tx(t.Context(), func(tx *store.Tx) error {
		var err error
		got, err = tx.SandboxPass()
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("a rediscovery that closed every gateway recorded nothing because the " +
			"controller was stopping; the gateways stay closed across the restart")
	}
	if !strings.Contains(got.Error, "the probe could not run") {
		t.Errorf("recorded %q; it has to carry why, which is what an operator acts on", got.Error)
	}
	if got.At.IsZero() {
		t.Error("recorded no time; a pass that stopped running is only visible as a " +
			"timestamp that stopped moving")
	}
}

// TestASuccessfulSandboxPassClearsTheReason: recovery has to be
// readable. A sandbox that reopened while still reporting the failure it
// recovered from sends an operator after a problem that is over.
func TestASuccessfulSandboxPassClearsTheReason(t *testing.T) {
	h := newHarness(t, 1)
	h.srv.recordSandboxPass(t.Context(), errors.New("the probe could not run"))
	h.srv.recordSandboxPass(t.Context(), nil)

	var got *store.SandboxPass
	if err := h.srv.store.Tx(t.Context(), func(tx *store.Tx) error {
		var err error
		got, err = tx.SandboxPass()
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Error != "" {
		t.Errorf("after a pass that succeeded the record is %+v; a sandbox that "+
			"recovered would keep reporting the failure it recovered from", got)
	}
}
