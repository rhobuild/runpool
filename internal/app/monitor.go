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

	// level is the pressure in force. Read on the admission path from
	// another goroutine, so it is atomic rather than guarded.
	level atomic.Int32
}

func newDiskMonitor(cfg *config.Config, log *slog.Logger, st *store.Store, probe diskProbe,
	lanes *cache.LaneManager, gate admissionGate, probeImage string) *diskMonitor {
	return &diskMonitor{
		log: log, store: st, probe: probe, lanes: lanes, gate: gate,
		probeImage: probeImage, policy: diskPolicyFrom(cfg),
	}
}

func (m *diskMonitor) current() disk.Level { return disk.Level(m.level.Load()) }

// resume loads the persisted level so a restart resumes the emergency
// that was in force instead of admitting into it. It must run before any
// binding serves, because it is what holds the credit pool shut.
func (m *diskMonitor) resume(ctx context.Context) error {
	return m.store.Tx(ctx, func(tx *store.Tx) error {
		p, err := tx.Pressure()
		if err != nil || p == nil {
			return err
		}
		level, err := disk.ParseLevel(p.Level)
		if err != nil {
			return err
		}
		m.level.Store(int32(level))
		m.gate.Hold(level.AdmissionClosed())
		if level != disk.Normal {
			m.log.Warn("resuming under disk pressure", "level", level.String())
		}
		return nil
	})
}

// run is the pressure loop: measure, decide, persist, act.
func (m *diskMonitor) run(ctx context.Context) {
	ticker := time.NewTicker(monitorInterval)
	defer ticker.Stop()
	for {
		m.pass(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// pass runs one measurement. A failed measurement changes nothing: the
// level in force stays in force, because losing the probe must not
// reopen an admission an emergency closed.
func (m *diskMonitor) pass(ctx context.Context) {
	facts, err := m.measure(ctx)
	if err != nil {
		m.log.Warn("disk measurement failed; keeping the current pressure level",
			"level", m.current().String(), "error", err)
		return
	}

	prev := m.current()
	next := disk.Next(prev, facts, m.policy.thresholds)
	m.level.Store(int32(next))
	// Credit follows admission: capacity that will not be served must
	// not be announced, or the broker assigns work that then waits on a
	// full disk. Running capsules keep their credit either way.
	m.gate.Hold(next.AdmissionClosed())

	if err := m.store.Tx(ctx, func(tx *store.Tx) error {
		if err := tx.SetPressure(store.PressureInfo{
			Level:        next.String(),
			FreeBytes:    facts.FreeBytes,
			FreeInodes:   facts.FreeInodes,
			ManagedBytes: facts.ManagedBytes,
		}); err != nil {
			return err
		}
		if next != prev {
			return tx.RecordAudit("disk-monitor", "pressure_"+next.String(), "host",
				fmt.Sprintf("was=%s free_bytes=%d managed_bytes=%d", prev, facts.FreeBytes, facts.ManagedBytes))
		}
		return nil
	}); err != nil {
		m.log.Error("cannot persist the pressure verdict", "error", err)
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
	pass := store.SandboxPass{At: time.Now().Unix()}
	if cause != nil {
		pass.Error = cause.Error()
	}
	if err := s.store.Tx(ctx, func(tx *store.Tx) error {
		return tx.SetSandboxPass(pass)
	}); err != nil {
		s.log.Error("cannot record the sandbox rediscovery outcome", "error", err)
	}
}
