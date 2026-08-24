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
// unrelated encodings that happen to be at the same number, and the
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

// Resolution says exactly what was observed and what was decided, never
// more. "Executed" is not in the vocabulary on purpose: the provider
// keeps the truth about the workload's functional result, and Runpool
// records only its own observations and decisions.
//
// It is a named type because it crosses three packages -- lease decides
// it, store records it, command reports it -- and it travels beside two
// free-text strings whose transposition the compiler could not see: a
// reason written into the vocabulary column reads as an outcome nothing
// in the vocabulary can answer for.
type Resolution string

const (
	// Unresolved is the zero value: nothing has been decided. An attempt
	// carries it from the moment it is recorded until something settles
	// it, and the empty literal is spelled here and nowhere else.
	Unresolved Resolution = ""
	// ResolutionCompletedObserved: the runtime was seen to exit.
	ResolutionCompletedObserved Resolution = "completed_observed"
	// ResolutionStartedObserved: the runtime was seen running; its exit
	// was not observed locally.
	ResolutionStartedObserved Resolution = "started_observed"
	// ResolutionMayHaveExecuted: a start was authorized and nothing
	// could prove the outcome either way; settled to honour
	// at-most-once.
	ResolutionMayHaveExecuted Resolution = "may_have_executed"
	// ResolutionRemoteCanceled: the provider canceled the workload.
	ResolutionRemoteCanceled Resolution = "remote_canceled"
	// ResolutionSuperseded: a newer delivery of the same workload
	// replaced this attempt before it started anything.
	ResolutionSuperseded Resolution = "superseded"
)

// AllResolutions is every decision Runpool can reach, for the tests that
// hold the vocabulary against the schema and against the machine that
// produces it.
var AllResolutions = []Resolution{
	ResolutionCompletedObserved,
	ResolutionStartedObserved,
	ResolutionMayHaveExecuted,
	ResolutionRemoteCanceled,
	ResolutionSuperseded,
}

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
	// observations only. It is opaque: the domain reports it and never
	// rules on it, because its words are the provider's to change.
	Result string
	// Canceled is that outcome translated -- the one thing the domain
	// decides differently for, and therefore the one thing that must
	// cross the boundary as a fact rather than a word. Read from Result
	// here, a provider that respelled it would stop cancellations
	// cancelling, silently, with every test still green.
	Canceled bool
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
	// NoObservation means this pass took no observation at all: nothing
	// asked the daemon or the capsule, so nothing is established and
	// nothing is contradicted. It does not carry the Observed prefix
	// because the six values that do each name what an observer saw, and
	// this names that there was no observer.
	//
	// It is the zero value, and it is what a cleanup retry arrives with:
	// the pass that measured is the pass that failed. What that pass
	// recorded on the lease is what a retry reads back, because this
	// value is not evidence against it.
	NoObservation ExecutionObservation = ""
	// ObservedCreated means the supervisor still holds the runner
	// unstarted. It is the capsule's own account of itself, reached
	// either by asking it or by reading the status it reserved for
	// stopping before the runner had the job.
	ObservedCreated ExecutionObservation = "created"
	// ObservedNeverStarted means the daemon has never started this
	// container. It says the same thing as ObservedCreated about the
	// job and something different about who is saying it: the host
	// daemon, which the workload has no socket to, where the capsule is
	// the machine running the workload. Nothing that weighs one account
	// against another may collapse the two.
	ObservedNeverStarted ExecutionObservation = "never_started"
	// ObservedRunning means the runner is executing now.
	ObservedRunning ExecutionObservation = "running"
	// ObservedExited means the runner ran and stopped.
	ObservedExited ExecutionObservation = "exited"
	// ObservedAbsent means the daemon has no such capsule.
	ObservedAbsent ExecutionObservation = "absent"
	// ObservedUnavailable means the runtime could not establish an outcome.
	ObservedUnavailable ExecutionObservation = "unavailable"
)

// Establishes reports whether this observation says anything about
// whether the workload began.
//
// Three do not, and they are the ones a retry produces: NoObservation
// because that pass measured nothing, ObservedAbsent because the capsule
// a measurement would have come from is the one the cleanup being
// retried has already removed, and ObservedUnavailable because the
// runtime could not be made to answer. None of them is evidence against
// a measurement an earlier pass took, which is why what a serving
// measures is kept rather than carried only by the goroutine that took
// it.
func (o ExecutionObservation) Establishes() bool {
	switch o {
	case ObservedCreated, ObservedNeverStarted, ObservedRunning, ObservedExited:
		return true
	default:
		return false
	}
}

// AllExecutionObservations is every value of this vocabulary, which is
// one more than the observations: NoObservation is a value and not an
// observation, and a switch that leaves it out decides for it by
// omission -- in the one function whose contract is that nothing is
// decided by omission.
//
// Anything deciding what to do with one has to decide for all of them,
// and a switch says nothing when a value is added: it falls into
// whatever branch is last, which reads as a decision and is not one. A
// value added here without a home elsewhere is what a totality check
// has to fail on.
var AllExecutionObservations = []ExecutionObservation{
	NoObservation,
	ObservedCreated,
	ObservedNeverStarted,
	ObservedRunning,
	ObservedExited,
	ObservedAbsent,
	ObservedUnavailable,
}
