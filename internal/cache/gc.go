package cache

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/rhobuild/runpool/internal/platform/docker"
	"github.com/rhobuild/runpool/internal/store"
)

// GC evicts cache lanes and nothing else. The universe is defined by
// the role label: only this instance's cache-lane volumes are ever
// candidates, so a leased workspace, a dind data volume or an open
// attempt's resources are structurally out of reach — they carry other
// roles and other owners. Within the universe, a leased lane is never
// taken: the row-first delete refuses it atomically, which also closes
// the race where a lane is leased between planning and execution.

// GCOptions bound one collection pass.
type GCOptions struct {
	// TTL evicts free lanes not used for this long. Zero disables TTL
	// eviction.
	TTL time.Duration
	// TargetBytes evicts free lanes, least recently used first, until
	// the managed total is at or under this. Negative disables the
	// watermark pass.
	TargetBytes int64
	// AllFree evicts every free lane whatever it measures, which is the
	// soft-emergency posture. It is a separate field rather than a target
	// of zero because a byte target cannot express it: a volume the
	// daemon cannot size counts as zero bytes, so a pool of unsized lanes
	// is already at any zero target and the watermark pass would plan
	// nothing in the one posture that asks for everything.
	AllFree bool
	// Now anchors TTL arithmetic so a plan is reproducible.
	Now time.Time
}

// Eviction is one planned removal and the reason it was chosen —
// "ttl", "lru", "emergency" or "orphan". Orphans are labeled lane volumes without a
// row: a crash between DeleteLane's row delete and volume removal
// leaves one, and only this sweep can find it.
type Eviction struct {
	LaneID           string
	Volume           string
	SourceProjectKey string
	Generation       string
	Reason           string
	Bytes            int64
	Orphan           bool
}

// GCPlan is what a pass would do, computed before anything is touched:
// the CLI prints it as the dry run, and apply executes exactly it.
type GCPlan struct {
	Evictions []Eviction
	// ManagedBytes is the measured total before eviction; KeptBytes
	// what remains if every eviction succeeds. Volumes whose size the
	// daemon could not compute count as zero in both, so the watermark
	// pass treats unknown as unreclaimable rather than guessing.
	ManagedBytes int64
	KeptBytes    int64
}

// PlanGC measures and decides. The daemon is listed before the store
// on purpose: a volume is created only after its row committed, so a
// volume seen now whose row is absent in the later read is a true
// orphan — the reverse order would misread every lane created between
// the two reads as one.
func (m *LaneManager) PlanGC(ctx context.Context, opts GCOptions) (GCPlan, error) {
	usage, err := m.volumes.OwnedVolumeUsage(ctx, m.instanceID)
	if err != nil {
		return GCPlan{}, fmt.Errorf("measure volumes: %w", err)
	}
	sizes := map[string]int64{} // lane id -> bytes
	orphaned := map[string]int64{}
	for _, u := range usage {
		if u.Labels[docker.LabelRole] != RoleCacheLane {
			continue
		}
		size := u.Size
		if size < 0 {
			size = 0
		}
		if lane := u.Labels[LabelLane]; lane != "" {
			sizes[lane] = size
		} else {
			orphaned[u.Name] = size
		}
	}

	var lanes []store.CacheLaneInfo
	if err := m.store.Tx(ctx, func(tx *store.Tx) error {
		lanes, err = tx.CacheLanes()
		return err
	}); err != nil {
		return GCPlan{}, err
	}

	var plan GCPlan
	known := map[string]bool{}
	var free []store.CacheLaneInfo
	for _, lane := range lanes {
		known[lane.ID] = true
		plan.ManagedBytes += sizes[lane.ID]
		if lane.LeasedBy == "" {
			free = append(free, lane)
		}
	}
	for _, u := range usage {
		if u.Labels[docker.LabelRole] != RoleCacheLane {
			continue
		}
		lane := u.Labels[LabelLane]
		if lane != "" && !known[lane] {
			orphaned[u.Name] = sizes[lane]
		}
	}

	evicted := map[string]bool{}
	evict := func(lane store.CacheLaneInfo, reason string) {
		evicted[lane.ID] = true
		plan.Evictions = append(plan.Evictions, Eviction{
			LaneID: lane.ID, Volume: VolumeName(lane.ID),
			SourceProjectKey: lane.SourceProjectKey, Generation: lane.Generation,
			Reason: reason, Bytes: sizes[lane.ID],
		})
	}

	if opts.TTL > 0 {
		cutoff := opts.Now.Add(-opts.TTL).Unix()
		for _, lane := range free {
			if lane.LastUsed < cutoff {
				evict(lane, "ttl")
			}
		}
	}

	if opts.AllFree {
		for _, lane := range free {
			if !evicted[lane.ID] {
				evict(lane, "emergency")
			}
		}
	} else if opts.TargetBytes >= 0 {
		remaining := plan.ManagedBytes
		for _, e := range plan.Evictions {
			remaining -= e.Bytes
		}
		// Deterministic LRU: oldest first, id as the tiebreak so two
		// plans over the same facts are the same plan.
		sort.Slice(free, func(i, j int) bool {
			if free[i].LastUsed != free[j].LastUsed {
				return free[i].LastUsed < free[j].LastUsed
			}
			return free[i].ID < free[j].ID
		})
		for _, lane := range free {
			if remaining <= opts.TargetBytes {
				break
			}
			if evicted[lane.ID] {
				continue
			}
			evict(lane, "lru")
			remaining -= sizes[lane.ID]
		}
	}

	for name, size := range orphaned {
		plan.Evictions = append(plan.Evictions, Eviction{
			Volume: name, Reason: "orphan", Bytes: size, Orphan: true,
		})
	}
	// Orphans in name order, after the lane passes: same facts, same plan.
	sort.SliceStable(plan.Evictions, func(i, j int) bool {
		if plan.Evictions[i].Orphan != plan.Evictions[j].Orphan {
			return !plan.Evictions[i].Orphan
		}
		if plan.Evictions[i].Orphan {
			return plan.Evictions[i].Volume < plan.Evictions[j].Volume
		}
		return false
	})

	plan.KeptBytes = plan.ManagedBytes
	for _, e := range plan.Evictions {
		if !e.Orphan {
			plan.KeptBytes -= e.Bytes
		}
	}
	return plan, nil
}

// GCResult is what actually happened: evictions applied, skipped
// because a lease took the lane between plan and apply, or failed with
// a retryable error.
type GCResult struct {
	Applied int
	Skipped int
	Failed  []error
	// AuditFailed are evictions that succeeded but whose audit row could
	// not be written. They are kept apart from Failed because the lane is
	// already gone: reporting them as failed evictions told the operator a
	// later pass would retry them, and no pass can — the lane is not in
	// any plan anymore. The trail has a hole; the collection worked.
	AuditFailed []error
}

// RunGC executes a plan. Every applied eviction is recorded in the
// audit log under the given actor; failures are collected, not fatal —
// the next pass retries what this one could not remove.
func (m *LaneManager) RunGC(ctx context.Context, plan GCPlan, actor string) (GCResult, error) {
	var res GCResult
	for _, e := range plan.Evictions {
		var err error
		if e.Orphan {
			err = m.removeOrphanVolume(ctx, e.Volume)
		} else {
			err = m.DeleteLane(ctx, e.LaneID)
		}
		switch {
		case errors.Is(err, store.ErrLaneBusy):
			// Leased between plan and apply: the lane won the race and
			// stays. Not a failure.
			res.Skipped++
			continue
		case err != nil:
			res.Failed = append(res.Failed, fmt.Errorf("evict %s: %w", e.Volume, err))
			continue
		}
		res.Applied++
		detail := fmt.Sprintf("reason=%s bytes=%d project=%s generation=%s",
			e.Reason, e.Bytes, e.SourceProjectKey, e.Generation)
		if err := m.store.Tx(ctx, func(tx *store.Tx) error {
			return tx.RecordAudit(actor, "gc_evict", e.Volume, detail)
		}); err != nil {
			res.AuditFailed = append(res.AuditFailed, fmt.Errorf("audit %s: %w", e.Volume, err))
		}
	}
	return res, nil
}

// removeOrphanVolume removes a labeled lane volume that has no row. No
// lease can reach it — rows are created before volumes and deleted
// before them too — but ownership is still proven first, because the
// volume namespace is shared.
func (m *LaneManager) removeOrphanVolume(ctx context.Context, name string) error {
	if _, err := m.volumes.OwnedIDByName(ctx, "volume", name, m.instanceID, ""); err != nil {
		return err
	}
	return m.volumes.RemoveVolume(ctx, name)
}

// GCTarget is the managed-byte ceiling a collection pass works down to:
// the low watermark as a fraction of the budget. Wanting every free lane
// gone is not a ceiling and is asked for with GCOptions.AllFree.
//
// It lives here, in one function, because it was computed in two places
// with the same arithmetic — the serving controller's monitor and the
// operator's `gc`. The command that owns the second copy documents the
// invariant the duplication broke: "a gc run must not invent thresholds
// the serving controller would not use". Two copies of a formula is how
// one of them eventually invents something.
func GCTarget(maxManagedBytes int64, lowPct int) int64 {
	return maxManagedBytes * int64(lowPct) / 100
}
