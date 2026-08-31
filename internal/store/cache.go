package store

import (
	"database/sql"
	"errors"
	"time"

	"github.com/rhobuild/runpool/internal/assignment"
	"github.com/rhobuild/runpool/internal/store/sqlitedb"
)

// CacheLane identifies one exclusive lane: its opaque id and the opaque
// project id and generation it is warm for. The lane's storage is a
// daemon-side volume named after the id; the store never touches it.
type CacheLane struct {
	ID         string
	ProjectID  string
	Generation string
}

// EnsureCacheProject maps a project's source key to a stable opaque id,
// minting one on first sight. The id, not the key, names the lane
// volumes, so a rename upstream never loses the cache.
func (t *Tx) EnsureCacheProject(sourceProjectKey string) (string, error) {
	id, err := t.q.GetCacheProjectID(t.ctx, sourceProjectKey)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	id = newID(8)
	if err := t.q.InsertCacheProject(t.ctx, sqlitedb.InsertCacheProjectParams{
		ID: id, SourceProjectKey: sourceProjectKey,
	}); err != nil {
		return "", err
	}
	return id, nil
}

// LeaseCacheLane hands a lease an exclusive lane for (project,
// generation): it reuses a free lane's warm data when one exists,
// otherwise mints a new lane while the pool is below maxLanes. It
// returns ErrNoLane when every lane is in use — the caller then runs
// without a cache rather than sharing one, since concurrent writers
// would corrupt it.
func (t *Tx) LeaseCacheLane(projectID, generation string, leaseID assignment.LeaseID, maxLanes int) (CacheLane, error) {
	id, err := t.q.FindFreeCacheLaneID(t.ctx, sqlitedb.FindFreeCacheLaneIDParams{
		ProjectID: projectID, Generation: generation,
	})
	switch {
	case err == nil:
		affected, err := t.q.ClaimCacheLane(t.ctx, sqlitedb.ClaimCacheLaneParams{
			LeaseID: requiredText(leaseID), ID: id,
		})
		if err != nil {
			return CacheLane{}, err
		}
		if affected == 0 {
			return CacheLane{}, ErrNoLane
		}
		return CacheLane{ID: id, ProjectID: projectID, Generation: generation}, nil
	case !errors.Is(err, sql.ErrNoRows):
		return CacheLane{}, err
	}

	count, err := t.q.CountCacheLanes(t.ctx, sqlitedb.CountCacheLanesParams{
		ProjectID: projectID, Generation: generation,
	})
	if err != nil {
		return CacheLane{}, err
	}
	if count >= int64(maxLanes) {
		return CacheLane{}, ErrNoLane
	}
	id = newID(8)
	if err := t.q.InsertCacheLane(t.ctx, sqlitedb.InsertCacheLaneParams{
		ID: id, ProjectID: projectID, Generation: generation, LeaseID: requiredText(leaseID),
	}); err != nil {
		return CacheLane{}, err
	}
	return CacheLane{ID: id, ProjectID: projectID, Generation: generation}, nil
}

// BackdateCacheLane moves a lane's LRU clock into the past. The runtime
// never calls it: it exists so a garbage-collection test can build lanes
// of different ages without sleeping through a real TTL, which is the
// only alternative at one-second clock granularity. It is a narrow typed
// operation rather than a general SQL escape hatch, because the escape
// hatch is what eventually gets used for something else.
func (t *Tx) BackdateCacheLane(laneID string, age time.Duration) error {
	affected, err := t.q.BackdateCacheLane(t.ctx, sqlitedb.BackdateCacheLaneParams{
		LastUsed: time.Now().Add(-age).Unix(), ID: laneID,
	})
	return mustAffect(affected, err)
}

// ReleaseCacheLane frees whatever lane a lease holds, leaving its data
// for the next lease. Releasing a lease that holds none is a no-op.
func (t *Tx) ReleaseCacheLane(leaseID assignment.LeaseID) error {
	return t.q.ReleaseCacheLane(t.ctx, requiredText(leaseID))
}

// DeleteCacheLane removes a lane's row so the id can never be handed out
// again. It refuses a leased lane: eviction must not race the job that
// is writing to the volume. The caller deletes the row first and removes
// the volume after — once the row is gone no lease can reach the lane,
// while the reverse order lets a fresh lease mount the name in the gap
// and have the daemon auto-create it without ownership labels. A crash
// between the two steps leaves a labeled orphan volume, which the GC
// sweep finds by exactly those labels.
func (t *Tx) DeleteCacheLane(laneID string) error {
	affected, err := t.q.DeleteFreeCacheLane(t.ctx, laneID)
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrLaneBusy
	}
	return nil
}

// ErrNoLane means every lane in a pool is currently leased.
var ErrNoLane = errors.New("no free cache lane")

// ErrLaneBusy means a lane could not be deleted because a lease holds it
// (or it does not exist, which for eviction purposes is the same: do not
// touch the volume).
var ErrLaneBusy = errors.New("cache lane is leased or unknown")
