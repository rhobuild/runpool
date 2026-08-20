package store

import "context"

// PurgeEverything removes the delivery and attempt machine during uninstall.
// No operation with a scope smaller than the whole instance may call it.
func (t *Tx) PurgeEverything() error {
	for _, purge := range []func(context.Context) error{
		t.q.PurgeResourceIntents,
		t.q.PurgeLeases,
		t.q.PurgeAttemptEvents,
		t.q.PurgeGitHubAttemptMetadata,
		t.q.PurgeAttempts,
		t.q.PurgeDeliveries,
		t.q.PurgeGitHubBindingMetadata,
		t.q.PurgeBindingContact,
		t.q.PurgeBindings,
		// Uninstall deletes the lane volumes, so the rows must go with
		// them. A lane row whose volume is gone still counts against the
		// project's lane ceiling and can never be reused, released or
		// deleted — its lease no longer exists to release it.
		t.q.PurgeCacheLanes,
		t.q.PurgeCacheProjects,
	} {
		if err := purge(t.ctx); err != nil {
			return err
		}
	}
	return nil
}
