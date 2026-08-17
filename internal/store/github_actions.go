package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"

	"github.com/rhobuild/runpool/internal/store/sqlitedb"
)

// RecordGitHubBindingMetadata stores the adapter's configuration for a
// binding, one row per neutral binding.
func (t *Tx) RecordGitHubBindingMetadata(bindingID int64, scope, canonicalURL, runnerGroup, scaleSetName string, scaleSetID int64) error {
	return t.q.UpsertGitHubBindingMetadata(t.ctx, sqlitedb.UpsertGitHubBindingMetadataParams{
		BindingID: bindingID, Scope: scope, CanonicalUrl: canonicalURL,
		RunnerGroup: runnerGroup, ScaleSetName: scaleSetName,
		ScaleSetID: sql.NullInt64{Int64: scaleSetID, Valid: scaleSetID > 0},
	})
}

// RecordGitHubAttemptMetadata stores the provider identifiers observed for an
// attempt. Runner request ids are diagnostic metadata and may be zero.
func (t *Tx) RecordGitHubAttemptMetadata(attemptID, jobID string, runnerRequestID, workflowRunID int64) error {
	return t.q.UpsertGitHubAttemptMetadata(t.ctx, sqlitedb.UpsertGitHubAttemptMetadataParams{
		AttemptID: attemptID, JobID: jobID,
		RunnerRequestID: runnerRequestID, WorkflowRunID: workflowRunID,
	})
}

// GitHubBinding is the adapter-owned configuration needed to address a scale
// set during reconciliation and uninstall.
type GitHubBinding struct {
	BindingID    int64
	Scope        string
	CanonicalURL string
	RunnerGroup  string
	ScaleSetName string
	ScaleSetID   int64
}

// GitHubBindings lists adapter metadata. Provider-neutral domain code reads
// provider_bindings instead and does not depend on this representation.
func (t *Tx) GitHubBindings() ([]GitHubBinding, error) {
	rows, err := t.q.ListGitHubBindingMetadata(t.ctx)
	if err != nil {
		return nil, err
	}
	out := make([]GitHubBinding, len(rows))
	for i, row := range rows {
		out[i] = GitHubBinding{
			BindingID: row.BindingID, Scope: row.Scope, CanonicalURL: row.CanonicalUrl,
			RunnerGroup: row.RunnerGroup, ScaleSetName: row.ScaleSetName,
			ScaleSetID: row.ScaleSetID.Int64,
		}
	}
	return out, nil
}

// GitHubScaleSetID returns the recorded ownership identity for a binding. Zero
// means that no scale set has been recorded.
func (t *Tx) GitHubScaleSetID(bindingID int64) (int64, error) {
	id, _, err := t.GitHubScaleSet(bindingID)
	return id, err
}

// GitHubScaleSet reports the recorded scale set id and whether this
// binding has a metadata row at all.
//
// The two answers differ where it matters. No row means this binding has
// never asked the provider for anything. A row with no id means it asked
// and did not learn the answer — it wrote down the name it was about to
// create and then failed, or was killed, before the provider's id came
// back. Only the second is grounds for taking over a set that already
// carries that name.
func (t *Tx) GitHubScaleSet(bindingID int64) (id int64, recorded bool, err error) {
	row, err := t.q.GetGitHubBindingMetadata(t.ctx, bindingID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return row.ScaleSetID.Int64, true, nil
}

// RecordGitHubRunnerID stores the ephemeral runner assigned to an attempt so
// failure cleanup can deregister it.
func (t *Tx) RecordGitHubRunnerID(attemptID string, runnerID int64) error {
	affected, err := t.q.SetGitHubAttemptRunner(t.ctx, sqlitedb.SetGitHubAttemptRunnerParams{
		RunnerID: runnerID, AttemptID: attemptID,
	})
	if err != nil {
		return err
	}
	if affected == 0 {
		return fmt.Errorf("%w: no GitHub metadata for attempt %s", ErrNotFound, attemptID)
	}
	return nil
}

// AttemptProviderReferences returns the provider's own identifiers for an
// attempt, as strings, for a report that has to point a person at the
// provider's UI. Zero values are omitted: a reference nobody recorded is
// worse than absent, because it reads as one that was.
//
// It is keyed by name rather than typed because its only consumer prints
// it. Anything deciding on these identifiers reads them through the
// accessors above, where they keep their meaning.
func (t *Tx) AttemptProviderReferences(attemptID string) (map[string]string, error) {
	row, err := t.q.GetGitHubAttemptMetadata(t.ctx, attemptID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	refs := map[string]string{}
	if row.JobID != "" {
		refs["job_id"] = row.JobID
	}
	for name, v := range map[string]int64{
		"workflow_run_id":   row.WorkflowRunID,
		"runner_request_id": row.RunnerRequestID,
		"runner_id":         row.RunnerID,
	} {
		if v != 0 {
			refs[name] = strconv.FormatInt(v, 10)
		}
	}
	if len(refs) == 0 {
		return nil, nil
	}
	return refs, nil
}

// GitHubRunnerID returns the recorded ephemeral runner. A missing metadata row
// reports zero because there is nothing to deregister.
func (t *Tx) GitHubRunnerID(attemptID string) (int64, error) {
	row, err := t.q.GetGitHubAttemptMetadata(t.ctx, attemptID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return row.RunnerID, nil
}
