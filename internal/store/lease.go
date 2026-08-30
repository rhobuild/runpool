package store

import (
	"slices"
	"time"

	"github.com/rhobuild/runpool/internal/assignment"
)

// LeaseState is one node of the capsule lease machine: the lifecycle of
// the host resources an attempt consumes, not the lifecycle of the work
// itself. What the workload did is the attempt's evidence, and reading a
// cleanup state as execution is how a job that never ran gets settled as
// if it had.
type LeaseState string

// A lease is born reserved: its admission credit is already held when
// the row is committed, so no earlier state can appear in the database.
const (
	LeaseReserved          LeaseState = "reserved"
	LeaseProvisioning      LeaseState = "provisioning"
	LeaseRuntimeRegistered LeaseState = "runtime_registered"
	LeaseWorkloadRunning   LeaseState = "workload_running"
	LeaseDraining          LeaseState = "draining"
	LeaseCleaning          LeaseState = "cleaning"
	LeaseReleased          LeaseState = "released"
	LeaseFailed            LeaseState = "failed"
	LeaseQuarantined       LeaseState = "quarantined"
)

// transitions is the whole state machine. Failure is reachable from
// every state that is still setting up or running, but not from cleaning:
// once a release is under way the outcome is released or quarantined, and
// a quarantined lease keeps consuming capacity until a cleaning retry
// resolves it. Released is the only terminal state.
var transitions = map[LeaseState][]LeaseState{
	LeaseReserved:          {LeaseProvisioning, LeaseFailed},
	LeaseProvisioning:      {LeaseRuntimeRegistered, LeaseFailed},
	LeaseRuntimeRegistered: {LeaseWorkloadRunning, LeaseDraining, LeaseFailed},
	LeaseWorkloadRunning:   {LeaseDraining, LeaseFailed},
	LeaseDraining:          {LeaseCleaning, LeaseFailed},
	LeaseCleaning:          {LeaseReleased, LeaseQuarantined},
	LeaseFailed:            {LeaseCleaning},
	LeaseQuarantined:       {LeaseCleaning},
}

func ValidTransition(from, to LeaseState) bool {
	for _, next := range transitions[from] {
		if next == to {
			return true
		}
	}
	return false
}

func (s LeaseState) Terminal() bool { return s == LeaseReleased }

// AllLeaseStates lists every state, including released.
var AllLeaseStates = []LeaseState{
	LeaseReserved, LeaseProvisioning, LeaseRuntimeRegistered,
	LeaseWorkloadRunning, LeaseDraining, LeaseCleaning, LeaseReleased,
	LeaseFailed, LeaseQuarantined,
}

// LiveLeaseStates is every state but released: the work an instance is
// still responsible for. Reporting, the reconciler's working set and the
// resource sweep all read it.
//
// It is derived rather than written out. A second list kept by hand is
// one edit away from disagreeing with the first, and the disagreement is
// silent and expensive: a live state missing here drops those leases out
// of the snapshot, so cleanup builds its keep set without them and
// deletes the resources of a capsule that is running.
var LiveLeaseStates = slices.DeleteFunc(slices.Clone(AllLeaseStates), LeaseState.Terminal)

// ReportedReleasedLeases is how much finished history a snapshot carries.
// A report wants recent history, not all of it — and the difference is
// the whole cost of `runpool status` on a host that has run for a year,
// because each lease in a snapshot costs two more queries.
//
// It is published: docs/reference/status-api.md states this number as the
// ceiling a consumer may rely on, so changing it changes the versioned
// document's contract and that page has to move with it.
const ReportedReleasedLeases = 50

// Lease is one accepted unit of work's runtime footprint: an admission
// credit is consumed when it reaches reserved, and every capsule resource
// it creates is recorded against it until cleanup releases them.
//
// It holds no provider identifiers. BindingID says whose runtime this is
// so the reconciler can find a client to clean up with; AttemptID is the
// link to the record of what the work actually did.
type Lease struct {
	ID          assignment.LeaseID
	BindingID   assignment.BindingID
	AttemptID   assignment.AttemptID
	TierID      assignment.TierID
	State       LeaseState
	RuntimeName assignment.RuntimeName
	// StartObservation is what this serving measured about whether the
	// workload began, kept because the measurement outlives the goroutine
	// that took it. A cleanup that has to be retried re-enters the
	// finalizing transaction with nothing in hand, and an attempt whose
	// capsule proved the runner never owned the job would otherwise settle
	// there as one that ran. Empty means this serving established nothing.
	StartObservation assignment.ExecutionObservation
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// ResourceKind is the container-engine object type of an owned capsule
// resource. It is persisted because the intent outlives the process that
// planned the external effect.
type ResourceKind string

const (
	ResourceContainer ResourceKind = "container"
	ResourceNetwork   ResourceKind = "network"
	ResourceVolume    ResourceKind = "volume"
)

var AllResourceKinds = []ResourceKind{
	ResourceContainer, ResourceNetwork, ResourceVolume,
}

// ResourceRole identifies the part of a capsule an intent owns. Only
// lease-scoped roles belong here; instance infrastructure and short-lived
// probes are discovered through engine ownership labels and never acquire a
// resource-intent row.
type ResourceRole string

const (
	ResourceRoleCapsule        ResourceRole = "capsule"
	ResourceRoleGateway        ResourceRole = "gateway"
	ResourceRoleCapsuleNetwork ResourceRole = "capsule-net"
	ResourceRoleDindData       ResourceRole = "dind-data"
)

var AllResourceRoles = []ResourceRole{
	ResourceRoleCapsule, ResourceRoleGateway,
	ResourceRoleCapsuleNetwork, ResourceRoleDindData,
}

// ResourceState is the durable state of one external-effect saga.
type ResourceState string

const (
	ResourcePlanned        ResourceState = "planned"
	ResourceCreating       ResourceState = "creating"
	ResourcePresent        ResourceState = "present"
	ResourceCleanupPending ResourceState = "cleanup_pending"
	ResourceDeleting       ResourceState = "deleting"
)

var AllResourceStates = []ResourceState{
	ResourcePlanned, ResourceCreating, ResourcePresent,
	ResourceCleanupPending, ResourceDeleting,
}

// ResourceIntent is the durable plan for one external object, committed
// before the effect that creates it and deleted only when the object is
// proven gone. The deterministic Name is the recovery handle for the
// windows in which existence is ambiguous; DockerID is set once the
// object confirmed. Retries, LastError and NotBefore pace the periodic
// reconciler per resource.
type ResourceIntent struct {
	ID        assignment.ResourceIntentID
	LeaseID   assignment.LeaseID
	Kind      ResourceKind
	Role      ResourceRole
	Name      string
	DockerID  string
	State     ResourceState
	Retries   int64
	LastError string
	// NotBefore is zero when no retry delay is active.
	NotBefore time.Time
	CreatedAt time.Time
}

// Handle is what a removal call addresses: the confirmed id when the
// object reported one, otherwise the deterministic name — which is how
// an object created in the ambiguous window is still reachable.
func (r ResourceIntent) Handle() string {
	if r.DockerID != "" {
		return r.DockerID
	}
	return r.Name
}
