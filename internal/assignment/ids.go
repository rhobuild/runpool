package assignment

// The identifiers the control plane keys on.
//
// They are distinct types because every one of them is a string or an
// integer, and a function taking four of those accepts them in any
// order. Nothing here changes what is stored or what travels: the point
// is that a swap stops compiling, in signatures where it previously
// produced a query that matched nothing and reported an empty result
// rather than an error.
//
// They live in this package because it is the vocabulary both sides of
// the control plane already share — the store writes these values and
// the app reads them, and neither should have to import the other to
// name one. `ExecutionObservation` is here for exactly that reason.
//
// No String methods. `%s` and slog already render a named string type,
// and adding one invites `.String()` at call sites where a conversion is
// what the reader needs to see.
type (
	// AttemptID identifies one attempt to serve a workload. It outlives
	// the lease serving it: disposition happens after release.
	AttemptID string
	// LeaseID identifies the host resources one attempt consumes. It
	// travels beside AttemptID through the disposition paths, which is
	// where confusing the two costs the most.
	LeaseID string
	// BindingID is a binding's row id, the local key every delivery,
	// attempt and lease hangs off.
	BindingID int64
	// DeliveryID is one provider message made durable.
	DeliveryID int64
	// InstanceID identifies this controller among any sharing a daemon.
	// It is the ownership boundary every sweep respects.
	InstanceID string
	// TargetID is the operator's name for a place work arrives from.
	TargetID string
	// TierID is the operator's name for one envelope of resources.
	TierID string
	// SourceWorkloadKey is the provider's identity for a workload,
	// opaque here.
	SourceWorkloadKey string
	// SourceBindingKey is a binding's durable identity — the value the
	// binding row is keyed by. It survives the process.
	SourceBindingKey string
	// BindingKey is a binding's in-memory name, derived from
	// configuration on every start and never written down. It is used
	// for allocator accounting and log correlation.
	//
	// It and SourceBindingKey have shared one English name for long
	// enough that separating them is half the reason this file exists:
	// one keys rows that outlive the process, the other keys a map that
	// does not.
	BindingKey string
	// RuntimeID is the daemon's id for a container.
	RuntimeID string
	// RuntimeName is the name a runtime was created under, which is the
	// handle a late provider report correlates by.
	RuntimeName string
	// ResourceIntentID is one planned Docker object's row id.
	ResourceIntentID int64
)
