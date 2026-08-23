// Package cache manages persistent per-repository cache lanes. A lane
// is one named Docker volume a repository-scoped job mounts whole at
// /cache; its warm contents survive between jobs and are reused by the
// next lease, which is the whole point — cache is performance state,
// never correctness state, so any lane may be evicted safely.
//
// The volume model is what makes reuse independent of how the
// controller is deployed. The volume's name derives from the lane's opaque
// local id — never from repository text, which is attacker-influenced — and
// its labels carry the ownership and identity needed for reconciliation and
// garbage collection.
package cache

import (
	"context"
	"fmt"

	"github.com/rhobuild/runpool/internal/engine"
	"github.com/rhobuild/runpool/internal/store"

	"github.com/rhobuild/runpool/internal/assignment"
)

// Volume labels a cache lane carries, alongside the instance ownership
// pair every managed object has. RoleCacheLane distinguishes persistent
// lanes from a lease's ephemeral volumes in every sweep: a lane belongs
// to the instance, not to any lease.
const (
	// LabelProject carries the opaque source-project id a lane is warm for.
	LabelProject   = "io.runpool.cache.project"
	LabelGen       = "io.runpool.cache.generation"
	LabelLane      = "io.runpool.cache.lane"
	RoleCacheLane  = "cache-lane"
	volumePrefix   = "runpool-cache-"
	labelRoleValue = RoleCacheLane
)

// LaneMount tells a capsule which volume its lane is. There is no path
// and no subpath left to resolve: the daemon mounts the whole volume.
type LaneMount struct {
	Volume string

	// Identity of the lane handed over, logged so a cache miss can be
	// traced to the lane it should have hit rather than guessed at.
	ProjectID  string
	LaneID     string
	Generation string
}

// laneVolumes is what the manager needs from Docker: make a lane's
// volume exist under proven ownership, prove ownership before deletion,
// remove it — fail-closed on anyone else's volume throughout — and
// measure what this instance's volumes weigh, for the GC planner.
type laneVolumes interface {
	EnsureOwnedVolume(ctx context.Context, name string, labels map[string]string) error
	OwnedIDByName(ctx context.Context, kind, name string,
		instanceID assignment.InstanceID, leaseID assignment.LeaseID) (string, error)
	RemoveVolume(ctx context.Context, name string) error
	OwnedVolumeUsage(ctx context.Context, instanceID assignment.InstanceID) ([]engine.VolumeUsage, error)
}

type LaneManager struct {
	store      *store.Store
	volumes    laneVolumes
	instanceID assignment.InstanceID
}

// New returns a cache manager. It touches no filesystem: lanes are
// daemon-side volumes, and the store carries their exclusivity.
func New(st *store.Store, volumes laneVolumes, instanceID assignment.InstanceID) *LaneManager {
	return &LaneManager{store: st, volumes: volumes, instanceID: instanceID}
}

// VolumeName is the deterministic, opaque name of a lane's volume.
func VolumeName(laneID string) string { return volumePrefix + laneID }

// Acquire leases an exclusive lane for a repository job and ensures its
// volume exists. It returns ok=false with no error when the pool is
// momentarily exhausted; the job then runs without a cache rather than
// corrupting a shared one.
func (m *LaneManager) Acquire(ctx context.Context, sourceProjectKey, generation string,
	leaseID assignment.LeaseID, maxLanes int) (LaneMount, bool, error) {
	var lane store.CacheLane
	err := m.store.Tx(ctx, func(tx *store.Tx) error {
		projectID, err := tx.EnsureCacheProject(sourceProjectKey)
		if err != nil {
			return err
		}
		lane, err = tx.LeaseCacheLane(projectID, generation, leaseID, maxLanes)
		return err
	})
	if err == store.ErrNoLane {
		return LaneMount{}, false, nil
	}
	if err != nil {
		return LaneMount{}, false, err
	}

	name := VolumeName(lane.ID)
	// The lane's own three go on top of the ownership every managed
	// object carries: what a lane is warm for is the cache's vocabulary,
	// not the daemon's.
	labels := engine.Ownership{Instance: m.instanceID, Role: labelRoleValue}.Labels()
	labels[LabelProject] = lane.ProjectID
	labels[LabelGen] = lane.Generation
	labels[LabelLane] = lane.ID
	if err := m.volumes.EnsureOwnedVolume(ctx, name, labels); err != nil {
		// The lane lease must not outlive a volume that cannot be used:
		// give it back so the job runs uncached and the next acquire can
		// try again.
		rerr := m.store.Tx(ctx, func(tx *store.Tx) error {
			return tx.ReleaseCacheLane(leaseID)
		})
		if rerr != nil {
			return LaneMount{}, false, fmt.Errorf("ensure lane volume: %w (and the lane lease could not be returned: %v)", err, rerr)
		}
		return LaneMount{}, false, fmt.Errorf("ensure lane volume: %w", err)
	}
	return LaneMount{
		Volume:    name,
		ProjectID: lane.ProjectID, LaneID: lane.ID, Generation: lane.Generation,
	}, true, nil
}

// DeleteLane evicts an unleased lane. The row is deleted first so the
// id can never be handed to a lease again, then the volume — proving
// ownership before touching it, because the volume namespace is shared
// with the world and a name collision must not delete someone else's
// data. A leased lane refuses with store.ErrLaneBusy; a crash between
// the two steps leaves a labeled volume the GC sweep can find.
func (m *LaneManager) DeleteLane(ctx context.Context, laneID string) error {
	err := m.store.Tx(ctx, func(tx *store.Tx) error {
		return tx.DeleteCacheLane(laneID)
	})
	if err != nil {
		return err
	}
	name := VolumeName(laneID)
	if _, err := m.volumes.OwnedIDByName(ctx, "volume", name, m.instanceID, ""); err != nil {
		return fmt.Errorf("lane volume %s: %w", name, err)
	}
	return m.volumes.RemoveVolume(ctx, name)
}
