package store

import (
	"database/sql"
	"errors"
	"time"
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
	var id string
	err := t.tx.QueryRow(`SELECT id FROM cache_projects WHERE source_project_key = ?`, sourceProjectKey).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	id = newID(8)
	if _, err := t.tx.Exec(`INSERT INTO cache_projects (id, source_project_key) VALUES (?, ?)`, id, sourceProjectKey); err != nil {
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
func (t *Tx) LeaseCacheLane(projectID, generation, leaseID string, maxLanes int) (CacheLane, error) {
	var id string
	err := t.tx.QueryRow(
		`SELECT id FROM cache_lanes WHERE project_id = ? AND generation = ? AND leased_by IS NULL
		 ORDER BY last_used DESC LIMIT 1`, projectID, generation).Scan(&id)
	switch {
	case err == nil:
		if _, err := t.tx.Exec(
			`UPDATE cache_lanes SET leased_by = ?, last_used = unixepoch() WHERE id = ?`, leaseID, id); err != nil {
			return CacheLane{}, err
		}
		return CacheLane{ID: id, ProjectID: projectID, Generation: generation}, nil
	case !errors.Is(err, sql.ErrNoRows):
		return CacheLane{}, err
	}

	var count int
	if err := t.tx.QueryRow(
		`SELECT count(*) FROM cache_lanes WHERE project_id = ? AND generation = ?`, projectID, generation).Scan(&count); err != nil {
		return CacheLane{}, err
	}
	if count >= maxLanes {
		return CacheLane{}, ErrNoLane
	}
	id = newID(8)
	if _, err := t.tx.Exec(
		`INSERT INTO cache_lanes (id, project_id, generation, leased_by) VALUES (?, ?, ?, ?)`,
		id, projectID, generation, leaseID); err != nil {
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
	return t.mustAffect(t.tx.Exec(
		`UPDATE cache_lanes SET last_used = unixepoch() - ? WHERE id = ?`,
		int64(age.Seconds()), laneID))
}

// ReleaseCacheLane frees whatever lane a lease holds, leaving its data
// for the next lease. Releasing a lease that holds none is a no-op.
func (t *Tx) ReleaseCacheLane(leaseID string) error {
	_, err := t.tx.Exec(`UPDATE cache_lanes SET leased_by = NULL WHERE leased_by = ?`, leaseID)
	return err
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
	res, err := t.tx.Exec(`DELETE FROM cache_lanes WHERE id = ? AND leased_by IS NULL`, laneID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
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
