package githubactions

import (
	"testing"

	"github.com/actions/scaleset"
)

// TestMatrixJobsAreDistinct: a workflow run holds many jobs — a matrix,
// or any workflow with parallel jobs, such as a three-job gate.
// Deduplicating on the workflow run therefore discarded every job after
// the first, silently and with no error anywhere. The job id is what
// becomes the workload key, and it is what redelivery must be keyed on.
func TestMatrixJobsAreDistinct(t *testing.T) {
	const sharedRun = int64(9001)
	msg := &scaleset.RunnerScaleSetMessage{
		MessageID: 1,
		JobAssignedMessages: []*scaleset.JobAssigned{
			{JobMessageBase: scaleset.JobMessageBase{
				OwnerName: "acme", RepositoryName: "app",
				JobID: "job-linux", WorkflowRunID: sharedRun,
			}},
			{JobMessageBase: scaleset.JobMessageBase{
				OwnerName: "acme", RepositoryName: "app",
				JobID: "job-macos", WorkflowRunID: sharedRun,
			}},
		},
	}

	out, _ := translate(msg)
	if len(out.Assigned) != 2 {
		t.Fatalf("assigned = %d; want both matrix jobs", len(out.Assigned))
	}
	if out.Assigned[0].SourceRunID != out.Assigned[1].SourceRunID {
		t.Fatal("fixture is wrong: both jobs must share one workflow run")
	}
	if out.Assigned[0].SourceWorkloadKey == out.Assigned[1].SourceWorkloadKey {
		t.Fatal("workload identity did not survive translation; deduplication would drop one")
	}

	// What the scheduler does with them: distinct identities are distinct
	// work, a repeated identity is a redelivery.
	seen := map[string]bool{}
	admitted := 0
	for _, d := range append(out.Assigned, out.Assigned[0]) { // last is a redelivery
		if d.SourceWorkloadKey != "" && seen[d.SourceWorkloadKey] {
			continue
		}
		seen[d.SourceWorkloadKey] = true
		admitted++
	}
	if admitted != 2 {
		t.Errorf("admitted %d jobs; want the two distinct ones, redelivery ignored", admitted)
	}
}
