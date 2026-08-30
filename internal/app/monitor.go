package app

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/rhobuild/runpool/internal/cache"
	"github.com/rhobuild/runpool/internal/config"
	"github.com/rhobuild/runpool/internal/disk"
	"github.com/rhobuild/runpool/internal/engine"
	"github.com/rhobuild/runpool/internal/store"

	"github.com/rhobuild/runpool/internal/assignment"
)

// The disk monitor measures once a minute. Pressure moves at the speed
// jobs write, and a probe container per pass is the cost; tighter would
// buy noise, looser would let a runaway job outrun the soft floor.
const monitorInterval = time.Minute

// diskProbe is the slice of the daemon a pressure verdict is made of.
// Defined here because the consumer owns its interfaces, and narrow
// because that is what lets a test present a filesystem with no room
// left — the one state the daemon cannot be asked to be in.
type diskProbe interface {
	ProbeFilesystemFree(ctx context.Context, image string, instanceID assignment.InstanceID) (engine.FilesystemFree, error)
	OwnedVolumeUsage(ctx context.Context, instanceID assignment.InstanceID) ([]engine.VolumeUsage, error)
}

// admissionGate is the half of the credit pool a verdict acts on.
// Capacity that will not be served must not be announced, or the broker
// assigns work that then waits on a full disk.
type admissionGate interface{ Hold(bool) }

// diskPolicy is the configured pressure policy, captured once at start.
type diskPolicy struct {
	thresholds disk.Thresholds
	ttl        time.Duration
}

// pressureWriter is the single durable boundary of a pressure verdict.
// Keeping it injectable lets the ordering tests fail that write without
// replacing the store or weakening the monitor's other dependencies.
type pressureWriter func(context.Context, disk.Level, disk.Level, disk.Facts, string) error

func diskPolicyFrom(cfg *config.Config) diskPolicy {
	g := cfg.Cache.Global
	return diskPolicy{
		thresholds: disk.Thresholds{
			MaxManagedBytes:  int64(g.MaxManagedBytes),
			HighPct:          g.HighWatermarkPercent,
			LowPct:           g.LowWatermarkPercent,
			SoftFreeBytes:    int64(g.SoftEmergencyFreeBytes),
			HardFreeBytes:    int64(g.HardEmergencyFreeBytes),
			ReserveFreeBytes: int64(cfg.Host.Reserve.FreeDisk),
		},
		ttl: time.Duration(cfg.Cache.Defaults.UnusedTTL),
	}
}

// diskMonitor owns the pressure level: it measures the filesystem,
// decides the level, persists it, closes admission when the level says
// so and collects cache lanes when the level obliges.
//
// The controller keeps only a read of the level in force, because every
// delivery consults it before admitting work. Everything that moves the
// level lives here.
type diskMonitor struct {
	log        *slog.Logger
	store      *store.Store
	probe      diskProbe
	lanes      *cache.LaneManager
	gate       admissionGate
	probeImage string
	policy     diskPolicy
	persist    pressureWriter

	// level is the pressure in force. Read on the admission path from
	// another goroutine, so it is atomic rather than guarded.
	level atomic.Int32
}

func newDiskMonitor(cfg *config.Config, log *slog.Logger, st *store.Store, probe diskProbe,
	lanes *cache.LaneManager, gate admissionGate, probeImage string) *diskMonitor {
	m := &diskMonitor{
		log: log, store: st, probe: probe, lanes: lanes, gate: gate,
		probeImage: probeImage, policy: diskPolicyFrom(cfg),
	}
	m.level.Store(int32(disk.Unknown))
	m.gate.Hold(true)
	m.persist = m.persistVerdict
	return m
}

func (m *diskMonitor) current() disk.Level { return disk.Level(m.level.Load()) }

// initialize restores a persisted emergency and obtains a fresh verdict
// before any provider session can announce capacity. A probe failure is
// not a startup failure: reconciliation must still be able to adopt work
// already running, while the unknown verdict keeps new work closed until
// a later pass succeeds.
func (m *diskMonitor) initialize(ctx context.Context) error {
	if err := m.resume(ctx); err != nil {
		return err
	}
	if err := m.pass(ctx); err != nil {
		m.log.Warn("initial disk verdict unavailable; admission remains closed", "error", err)
	}
	return nil
}

// resume restores only a persisted closed verdict. A normal or high row
// describes an earlier process and cannot prove that the daemon still has
// room now; opening admission requires a fresh verdict persisted by pass.
func (m *diskMonitor) resume(ctx context.Context) error {
	var persisted *store.PressureInfo
	if err := m.store.Tx(ctx, func(tx *store.Tx) error {
		p, err := tx.Pressure()
		persisted = p
		return err
	}); err != nil {
		return err
	}
	if persisted == nil {
		return nil
	}
	level, err := disk.ParseLevel(persisted.Level)
	if err != nil {
		return err
	}
	if !level.AdmissionClosed() {
		m.log.Info("persisted disk verdict awaits a fresh measurement", "level", level.String())
		return nil
	}
	m.level.Store(int32(level))
	m.gate.Hold(true)
	if level != disk.Unknown {
		m.log.Warn("resuming under disk pressure", "level", level.String())
	}
	return nil
}

// run continues the pressure loop after initialize performed the startup
// pass. Waiting for the first tick avoids paying for the same daemon probe
// twice during every start.
func (m *diskMonitor) run(ctx context.Context) {
	ticker := time.NewTicker(monitorInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.pass(ctx)
		}
	}
}

// pass measures, decides, and commits one verdict. Closing is applied
// before persistence so a failed store cannot leave unsafe capacity open;
// opening is persisted first so a restart cannot observe an uncommitted
// recovery as permission to admit.
func (m *diskMonitor) pass(ctx context.Context) error {
	facts, err := m.measure(ctx)
	if err != nil {
		prev := m.current()
		if prev != disk.SoftEmergency && prev != disk.HardEmergency {
			m.level.Store(int32(disk.Unknown))
			m.gate.Hold(true)
			unknown := disk.Facts{FreeBytes: -1, FreeInodes: -1, ManagedBytes: -1}
			if persistErr := m.persist(ctx, prev, disk.Unknown, unknown, err.Error()); persistErr != nil {
				m.log.Error("cannot persist an unavailable disk verdict", "error", persistErr)
			}
		}
		m.log.Warn("disk measurement failed; admission remains closed",
			"level", m.current().String(), "error", err)
		return err
	}

	prev := m.current()
	next := disk.Next(prev, facts, m.policy.thresholds)
	requiresDurableOpen := prev.AdmissionClosed() && !next.AdmissionClosed()
	if !requiresDurableOpen {
		m.apply(next)
	}
	if err := m.persist(ctx, prev, next, facts, ""); err != nil {
		m.log.Error("cannot persist the pressure verdict", "error", err)
		if next.WantsGC() {
			m.collectGarbage(ctx, next)
		}
		return err
	}
	if requiresDurableOpen {
		m.apply(next)
	}

	if next != prev {
		logf := m.log.Info
		if next >= disk.SoftEmergency || prev >= disk.SoftEmergency {
			logf = m.log.Warn
		}
		if next == disk.HardEmergency {
			// The alert: fail closed and say so as loudly as a log can.
			logf = m.log.Error
		}
		logf("disk pressure changed", "from", prev.String(), "to", next.String(),
			"free_bytes", facts.FreeBytes, "managed_bytes", facts.ManagedBytes,
			"admission", map[bool]string{true: "closed", false: "open"}[next.AdmissionClosed()])
	}

	if next.WantsGC() {
		m.collectGarbage(ctx, next)
	}
	return nil
}

// apply changes the in-memory verdict and its allocator gate as one
// monitor operation. Delivery also reads the level, so storing a closed
// level first makes the transition safe even before Hold reaches every
// capacity report.
func (m *diskMonitor) apply(level disk.Level) {
	m.level.Store(int32(level))
	m.gate.Hold(level.AdmissionClosed())
}

func (m *diskMonitor) persistVerdict(ctx context.Context, prev, next disk.Level, facts disk.Facts, detail string) error {
	return m.store.Tx(ctx, func(tx *store.Tx) error {
		if err := tx.SetPressure(store.PressureVerdict{
			Level:        next.String(),
			FreeBytes:    facts.FreeBytes,
			FreeInodes:   facts.FreeInodes,
			ManagedBytes: facts.ManagedBytes,
		}); err != nil {
			return err
		}
		if next == prev {
			return nil
		}
		auditDetail := fmt.Sprintf("was=%s free_bytes=%d managed_bytes=%d", prev, facts.FreeBytes, facts.ManagedBytes)
		if detail != "" {
			auditDetail += " reason=" + detail
		}
		return tx.RecordAudit("disk-monitor", "pressure_"+next.String(), "host", auditDetail)
	})
}

// measure probes the daemon's filesystem from inside it and weighs this
// instance's cache lanes.
func (m *diskMonitor) measure(ctx context.Context) (disk.Facts, error) {
	free, err := m.probe.ProbeFilesystemFree(ctx, m.probeImage, m.store.InstanceID())
	if err != nil {
		return disk.Facts{}, fmt.Errorf("filesystem probe: %w", err)
	}
	usage, err := m.probe.OwnedVolumeUsage(ctx, m.store.InstanceID())
	if err != nil {
		return disk.Facts{}, fmt.Errorf("volume usage: %w", err)
	}
	var managed int64
	for _, u := range usage {
		if u.Role == engine.RoleCacheLane && u.Size > 0 {
			managed += u.Size
		}
	}
	return disk.Facts{
		FreeBytes:    free.FreeBytes,
		FreeInodes:   free.FreeInodes,
		ManagedBytes: managed,
	}, nil
}

// collectGarbage runs the pass the level obliges: at high pressure,
// down to the low watermark; in a soft emergency, every free lane.
// Failures are logged and retried by the next pass.
func (m *diskMonitor) collectGarbage(ctx context.Context, level disk.Level) {
	opts := cache.GCOptions{
		TTL: m.policy.ttl,
		TargetBytes: cache.GCTarget(m.policy.thresholds.MaxManagedBytes,
			m.policy.thresholds.LowPct),
		AllFree: level.Aggressive(),
		Now:     time.Now(),
	}
	plan, err := m.lanes.PlanGC(ctx, opts)
	if err != nil {
		m.log.Error("gc planning failed", "error", err)
		return
	}
	if len(plan.Evictions) == 0 {
		return
	}
	res, err := m.lanes.RunGC(ctx, plan, "disk-monitor")
	if err != nil {
		m.log.Error("gc failed", "error", err)
		return
	}
	m.log.Info("gc pass", "level", level.String(), "applied", res.Applied,
		"skipped", res.Skipped, "failed", len(res.Failed),
		"managed_bytes", plan.ManagedBytes, "kept_bytes", plan.KeptBytes)
	for _, ferr := range res.Failed {
		m.log.Warn("gc eviction failed; the next pass retries", "error", ferr)
	}
}

// currentPressure is the level in force, which every delivery consults
// before admitting work. It is the whole of the monitor the controller
// takes part in.
func (s *Controller) currentPressure() disk.Level { return s.disk.current() }

// sandboxRecordBudget bounds the detached write below. Five seconds
// because it is one upsert of two rows against a local file, and well
// inside LoopStopBudget, whose claim about what a stopping loop may cost
// this must not be the thing that falsifies.
const sandboxRecordBudget = 5 * time.Second

// recordSandboxPass writes down what the egress sandbox's last
// rediscovery concluded.
//
// The pass already logs, and a log is not enough for this one. A
// rediscovery that cannot be trusted closes every gateway to all egress
// -- correct, and also every running job losing its network at once,
// indefinitely, for as long as discovery keeps failing. The same failure
// refuses to let serve start at all; once running it was invisible to
// `runpool status`, which is the document an operator is sent to.
//
// The write is best effort and says so: a store that will not take this
// must not stop the loop that maintains the policy. The failure is
// already logged by the pass itself, so nothing is lost that was not
// already lost.
func (s *Controller) recordSandboxPass(ctx context.Context, cause error) {
	pass := store.SandboxPass{At: time.Now()}
	if cause != nil {
		pass.Error = cause.Error()
	}
	// Detached, for the same reason closing the message sessions is: a
	// pass can conclude and shutdown begin before this runs, and the
	// context Watch was given is the one that just got cancelled. A
	// failure recorded nowhere because the process was stopping is the
	// failure most worth having recorded -- the gateways stay closed
	// across the restart, and the next pass is five minutes away.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sandboxRecordBudget)
	defer cancel()
	if err := s.store.Tx(writeCtx, func(tx *store.Tx) error {
		return tx.SetSandboxPass(pass)
	}); err != nil {
		s.log.Error("cannot record the sandbox rediscovery outcome", "error", err)
	}
}
