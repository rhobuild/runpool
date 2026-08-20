// Package assignment holds the provider-neutral vocabulary of the
// control plane: what a provider commits to a binding, and what it
// observes about that work afterwards. Provider identities cross this
// boundary as opaque keys — the domain validates that they are present
// and stable, never what they mean — so the durable schema, the
// scheduler and recovery hold properties Runpool owns, not properties
// borrowed from one provider's API.
package assignment

import (
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"
)

// WorkloadAssignment is one unit of work a provider committed to a
// binding. It is the domain's view of an assignment: every field is
// either an opaque provider key or observed correlation metadata.
type WorkloadAssignment struct {
	// SourceWorkloadKey is the provider's identity for the workload
	// itself, opaque and non-empty. It is the only field the domain
	// deduplicates on: grouping identities (a run, a batch) hold many
	// workloads, and keying on one of those collapses siblings.
	SourceWorkloadKey string
	// TenantKey and ProjectKey locate the workload's owner — for cache
	// scoping and diagnostics, never for identity.
	TenantKey  string
	ProjectKey string
	// SourceRequestID is the provider's attempt correlation number as
	// observed. Zero is a legitimate value; it is never a key.
	SourceRequestID int64
	// SourceRunID is the provider's grouping-run correlation as
	// observed. A run holds many workloads, so it is never a key.
	SourceRunID int64
	// Labels are the provider's routing labels, kept for diagnostics.
	Labels []string
}

// Validate reports whether the assignment can be made durable. An
// assignment with no workload key cannot be deduplicated or recovered,
// and recording it anyway plants a row nothing can recognise again.
func (a WorkloadAssignment) Validate() error {
	if a.SourceWorkloadKey == "" {
		return fmt.Errorf("assignment for %s/%s (run %d) carries no workload key and cannot be made durable",
			a.TenantKey, a.ProjectKey, a.SourceRunID)
	}
	return nil
}

// DeliveryKeyVersion prefixes the delivery key. It is named, and named
// separately from the binding key's version, because the two are
// unrelated encodings that happen to be at the same number — and the
// cost of bumping them is not the same.
//
// Bumping this one re-keys deliveries. A message already recorded stops
// matching, so it is processed again; the attempts it created are still
// there, and the redelivery finds them by workload key rather than
// creating a second set. Recoverable.
//
// Bumping the binding key's is a rename of every binding. See
// bindingKeyVersion in internal/app, which says what that costs, and do not treat the
// two as one value because they read alike.
const DeliveryKeyVersion = "v2"

// DeliveryKey encodes a provider's delivery identity as the opaque,
// versioned key the store deduplicates on. The version prefix is what
// lets the encoding evolve without two encodings of one delivery ever
// comparing equal.
//
// The queue's identity is part of it, because a delivery id is only
// unique within the queue that issued it. A queue destroyed upstream and
// recreated starts numbering again, and the binding outlives that: keyed
// on the id alone, a fresh message can collide with a delivery this
// binding already recorded and confirmed.
func DeliveryKey(sourceQueueID, sourceID int) string {
	return fmt.Sprintf("%s|%d|%d", DeliveryKeyVersion, sourceQueueID, sourceID)
}

// Fingerprint digests the normalized content of a delivery: every field
// of every assignment the domain processes, in a canonical order that
// does not depend on how the provider happened to arrange the message.
// A redelivery must reproduce it byte for byte; the same delivery key
// with a different fingerprint is contract drift, and the store fails
// closed on it.
func Fingerprint(assignments []WorkloadAssignment) [32]byte {
	lines := make([]string, len(assignments))
	for i, a := range assignments {
		labels := append([]string(nil), a.Labels...)
		sort.Strings(labels)
		lines[i] = fmt.Sprintf("%s|%s|%s|%d|%d|%s",
			a.SourceWorkloadKey, a.TenantKey, a.ProjectKey,
			a.SourceRequestID, a.SourceRunID, strings.Join(labels, ","))
	}
	sort.Strings(lines)
	return sha256.Sum256([]byte(strings.Join(lines, "\n")))
}

// LifecycleKind is what a provider observed about a workload.
type LifecycleKind string

const (
	// LifecycleStarted: the provider saw the workload begin executing.
	LifecycleStarted LifecycleKind = "started"
	// LifecycleCompleted: the provider saw the workload finish. It is a
	// hint — completion truth is the runtime exiting plus
	// reconciliation, and this observation can lag it by minutes.
	LifecycleCompleted LifecycleKind = "completed"
)

// Attempt resolutions say exactly what was observed and what was
// decided, never more. "Executed" is not in the vocabulary on purpose:
// the provider keeps the truth about the workload's functional result,
// and Runpool records only its own observations and decisions.
const (
	// ResolutionCompletedObserved: the runtime was seen to exit.
	ResolutionCompletedObserved = "completed_observed"
	// ResolutionStartedObserved: the runtime was seen running; its exit
	// was not observed locally.
	ResolutionStartedObserved = "started_observed"
	// ResolutionMayHaveExecuted: a start was authorized and nothing
	// could prove the outcome either way; settled to honour
	// at-most-once.
	ResolutionMayHaveExecuted = "may_have_executed"
	// ResolutionRemoteCanceled: the provider canceled the workload.
	ResolutionRemoteCanceled = "remote_canceled"
	// ResolutionSuperseded: a newer delivery of the same workload
	// replaced this attempt before it started anything.
	ResolutionSuperseded = "superseded"
)

// WorkloadLifecycleEvent is a provider observation about one workload.
// It carries the workload's own key: an event that names only the
// runtime cannot be correlated to the attempt it belongs to, which is
// how a cancellation aimed at an old attempt could hit a new one.
type WorkloadLifecycleEvent struct {
	Kind              LifecycleKind
	SourceWorkloadKey string
	TenantKey         string
	ProjectKey        string
	// RuntimeName identifies the provider-side runtime the observation
	// names, opaque to the domain. It is the handle a late report
	// correlates by, which is why the workload key travels beside it
	// rather than instead of it.
	RuntimeName RuntimeName
	// Result is the provider's stated outcome, populated on completed
	// observations only.
	Result string
}

// ExecutionObservation is what can be proven about a prepared runtime. It
// is an observation, never an inference; unavailable state refuses to
// guess.
//
// It lives in this package rather than beside the code that takes it
// because two sides need the vocabulary and neither should pay for the
// other: the capsule layer produces an observation from the daemon, and
// the lease machine consumes it to settle an attempt. Naming it here —
// where the lifecycle vocabulary already lives — is what lets the lease
// machine dispose of work without linking a container runtime.
type ExecutionObservation string

const (
	// ObservedCreated means the supervisor still holds the runner unstarted.
	ObservedCreated ExecutionObservation = "created"
	// ObservedRunning means the runner is executing now.
	ObservedRunning ExecutionObservation = "running"
	// ObservedExited means the runner ran and stopped.
	ObservedExited ExecutionObservation = "exited"
	// ObservedAbsent means the daemon has no such capsule.
	ObservedAbsent ExecutionObservation = "absent"
	// ObservedUnavailable means the runtime could not establish an outcome.
	ObservedUnavailable ExecutionObservation = "unavailable"
)
