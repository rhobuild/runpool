package githubactions

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/actions/scaleset"

	"github.com/rhobuild/runpool/internal/assignment"
)

func TestTranslate(t *testing.T) {
	base := func(owner, repo string, request int64) scaleset.JobMessageBase {
		return scaleset.JobMessageBase{
			OwnerName:       owner,
			RepositoryName:  repo,
			JobID:           fmt.Sprintf("job-%d", request),
			RunnerRequestID: request,
			WorkflowRunID:   request * 10,
			RequestLabels:   []string{"runpool-standard"},
		}
	}
	msg := &scaleset.RunnerScaleSetMessage{
		MessageID: 7,
		Statistics: &scaleset.RunnerScaleSetStatistic{
			TotalAssignedJobs: 2,
			TotalRunningJobs:  1,
		},
		JobAvailableMessages: []*scaleset.JobAvailable{
			{JobMessageBase: base("acme", "offered", 31)},
		},
		JobAssignedMessages: []*scaleset.JobAssigned{
			{JobMessageBase: base("acme", "direct", 0)},
		},
		JobStartedMessages: []*scaleset.JobStarted{
			{JobMessageBase: base("acme", "direct", 0), RunnerName: "r1", RunnerID: 11},
		},
		JobCompletedMessages: []*scaleset.JobCompleted{
			{JobMessageBase: base("acme", "direct", 0), RunnerName: "r1", RunnerID: 11, Result: "succeeded"},
		},
	}

	out, available := translate(msg)

	if out.ID != 7 {
		t.Errorf("id = %d", out.ID)
	}
	if out.Statistics == nil || out.Statistics.Assigned != 2 || out.Statistics.Running != 1 {
		t.Errorf("statistics = %+v", out.Statistics)
	}
	// Availables are not assigned until acquired: they come back apart.
	wantAvailable := []assignment.WorkloadAssignment{{
		SourceWorkloadKey: "job-31", TenantKey: "acme", ProjectKey: "offered",
		SourceRequestID: 31, SourceRunID: 310,
		Labels: []string{"runpool-standard"},
	}}
	if !reflect.DeepEqual(available, wantAvailable) {
		t.Errorf("available = %+v", available)
	}
	if len(out.Assigned) != 1 || out.Assigned[0].ProjectKey != "direct" {
		t.Errorf("assigned = %+v", out.Assigned)
	}
	// Lifecycle events carry the workload's own key alongside the
	// runtime: an event that names only the runtime cannot be correlated
	// to the attempt it belongs to.
	if len(out.Started) != 1 || out.Started[0].RuntimeName != "r1" ||
		out.Started[0].SourceWorkloadKey != "job-0" ||
		out.Started[0].Kind != assignment.LifecycleStarted || out.Started[0].Result != "" {
		t.Errorf("started = %+v", out.Started)
	}
	if len(out.Completed) != 1 || out.Completed[0].Result != "succeeded" ||
		out.Completed[0].SourceWorkloadKey != "job-0" ||
		out.Completed[0].Kind != assignment.LifecycleCompleted {
		t.Errorf("completed = %+v", out.Completed)
	}
}

func TestTranslateEmptyStatistics(t *testing.T) {
	out, available := translate(&scaleset.RunnerScaleSetMessage{MessageID: 1})
	if out.Statistics != nil || available != nil || out.Assigned != nil {
		t.Errorf("empty message translated to %+v / %+v", out, available)
	}
}
