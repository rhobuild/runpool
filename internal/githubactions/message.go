package githubactions

import (
	"github.com/actions/scaleset"

	"github.com/rhobuild/runpool/internal/assignment"
)

// Statistics is the remote demand snapshot. Assigned is the scaling
// source of truth per the upstream autoscaling contract; everything
// else is context.
type Statistics struct {
	Available         int
	Acquired          int
	Assigned          int
	Running           int
	RegisteredRunners int
	BusyRunners       int
	IdleRunners       int
}

func fromStatistics(s *scaleset.RunnerScaleSetStatistic) *Statistics {
	if s == nil {
		return nil
	}
	return &Statistics{
		Available:         s.TotalAvailableJobs,
		Acquired:          s.TotalAcquiredJobs,
		Assigned:          s.TotalAssignedJobs,
		Running:           s.TotalRunningJobs,
		RegisteredRunners: s.TotalRegisteredRunners,
		BusyRunners:       s.TotalBusyRunners,
		IdleRunners:       s.TotalIdleRunners,
	}
}

// Message is one translated broker message, already in the domain's
// provider-neutral terms: the adapter is where GitHub's job identities
// become opaque workload keys, and nothing GitHub-shaped travels
// further. A message may be delivered more than once (acknowledgement
// can fail after processing), so consumers must be idempotent.
type Message struct {
	ID         int
	Statistics *Statistics
	// Assigned holds the workloads the broker committed to this scale set
	// in the message itself. It is a function of the message id alone, so
	// a redelivery of the same id carries the same set.
	Assigned []assignment.WorkloadAssignment
	// Acquired holds availables this session claimed by calling the
	// broker. It is deliberately separate from Assigned: what an
	// acquisition grants depends on who else asked, so folding it into
	// Assigned would make a message's content vary between redeliveries
	// of the same id — which the store reads as the provider changing a
	// payload under a stable key, and refuses as unrecoverable drift.
	Acquired []assignment.WorkloadAssignment
	// StrandedGrants are request ids the broker granted that no offered
	// workload could receive — one it was never asked for, or one it
	// granted more times than it was offered. The job is claimed at the
	// broker with nothing here to run it, which is worth saying out loud:
	// it cannot be recovered from this side, and a silent drop is how it
	// would be discovered months later as a job that never ran.
	StrandedGrants []int64
	// AcquireError is set when this session offered availables and the
	// broker refused the call. The offers are not lost — an available
	// nobody acquires stays queued upstream and is offered again — so the
	// rest of the message is still delivered rather than discarded. It is
	// reported because a session that can never acquire is a session
	// whose availables never become work.
	AcquireError error
	Started      []assignment.WorkloadLifecycleEvent
	Completed    []assignment.WorkloadLifecycleEvent
}

// translate maps an upstream message. Availables come back separately
// so Poll can acquire them before merging — never observed live (while
// capacity is announced the broker assigns directly), but the protocol
// defines the shape and silently dropping it would strand jobs.
func translate(msg *scaleset.RunnerScaleSetMessage) (Message, []assignment.WorkloadAssignment) {
	out := Message{ID: msg.MessageID, Statistics: fromStatistics(msg.Statistics)}
	var available []assignment.WorkloadAssignment
	for _, j := range msg.JobAvailableMessages {
		available = append(available, workload(j.JobMessageBase))
	}
	for _, j := range msg.JobAssignedMessages {
		out.Assigned = append(out.Assigned, workload(j.JobMessageBase))
	}
	for _, j := range msg.JobStartedMessages {
		out.Started = append(out.Started, observation(j.JobMessageBase, j.RunnerName, ""))
	}
	for _, j := range msg.JobCompletedMessages {
		out.Completed = append(out.Completed, observation(j.JobMessageBase, j.RunnerName, j.Result))
	}
	return out, available
}

// workload maps one upstream job to the neutral assignment. JobID is
// the workload key: WorkflowRunID identifies the run that contains the
// job, and a run holds many jobs — a matrix, or any workflow with
// parallel jobs — so only JobID identifies the workload. The runner
// request id rides along as observed correlation, never identity.
func workload(b scaleset.JobMessageBase) assignment.WorkloadAssignment {
	return assignment.WorkloadAssignment{
		SourceWorkloadKey: b.JobID,
		TenantKey:         b.OwnerName,
		ProjectKey:        b.RepositoryName,
		SourceRequestID:   b.RunnerRequestID,
		SourceRunID:       b.WorkflowRunID,
		Labels:            b.RequestLabels,
	}
}

// resultCanceled is the provider's word for a workload the requester
// stopped. It is spelled once, here, because this is the only layer
// allowed to know it.
const resultCanceled = "canceled"

// observation maps a lifecycle message, keeping the workload's own key:
// events that named only the runner could not be correlated to the
// attempt they belong to, and a cancellation aimed at an old attempt
// could then hit a new one.
func observation(b scaleset.JobMessageBase, runnerName, result string) assignment.WorkloadLifecycleEvent {
	return assignment.WorkloadLifecycleEvent{
		SourceWorkloadKey: b.JobID,
		TenantKey:         b.OwnerName,
		ProjectKey:        b.RepositoryName,
		RuntimeName:       assignment.RuntimeName(runnerName),
		Result:            result,
		Canceled:          result == resultCanceled,
	}
}
