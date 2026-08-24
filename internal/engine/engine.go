// Package engine is the container engine as Runpool needs it: the
// vocabulary a domain package speaks when it asks for a capsule's
// network, a cache lane's volume or an execution's state.
//
// It holds no client and reaches nothing. An adapter under
// internal/engine implements it against one engine's API and is the only
// place that engine's own types appear; what leaves an adapter is these
// values, so a package that launches capsules never learns which daemon
// launched them.
//
// The split is not anticipation of a second engine. It is what makes the
// one there is testable: a live daemon cannot be told to be unreachable,
// to lose a container, or to refuse a name that is taken, and those are
// the answers that decide whether work is settled or held for a person.
package engine

import (
	"errors"
	"net/netip"

	"github.com/rhobuild/runpool/internal/assignment"
)

// Ownership labels: every resource Runpool creates is identifiable and
// reconcilable through the Docker API alone, so a crashed controller's
// objects can always be found without trusting its own books.
const (
	labelManaged  = "io.runpool.managed"
	labelInstance = "io.runpool.instance"
	labelKind     = "io.runpool.kind"
	labelLease    = "io.runpool.lease"
	labelRole     = "io.runpool.role"
	labelAttempt  = "io.runpool.attempt"
	labelTarget   = "io.runpool.target"
	labelTier     = "io.runpool.tier"
)

// ObjectKind is the kind of daemon object an ownership stamp describes,
// and the kind a name is resolved as.
//
// It is a named type because it crosses this port in both directions and
// is dispatched on at the far end: the adapter picks which inspect to
// call from it, so a value spelled a second time somewhere else is a
// resolution that silently finds nothing. The store carries the same
// vocabulary for what it persists, as store.ResourceKind, and the two
// meet only where an intent is written -- neither is derived from the
// other, because one is a fact about a running object and the other is a
// row that outlives it.
type ObjectKind string

const (
	KindContainer ObjectKind = "container"
	KindNetwork   ObjectKind = "network"
	KindVolume    ObjectKind = "volume"
)

// Ownership is what a Runpool instance stamps on everything it creates,
// and the only thing that proves an object is its to remove. A name is
// not proof: a foreign object can carry the name a plan expects, and
// adopting it by name is how a sweep deletes somebody else's work.
//
// It is one value rather than a map each caller builds, because the
// shape is fixed and the six places that built it by hand could each
// forget a key. What varies is which fields a kind of object has:
// instance infrastructure carries no lease, and a probe carries neither
// attempt nor tier. An unset field is left off rather than written
// empty, so an object states what is true of it and nothing else.
type Ownership struct {
	Instance assignment.InstanceID
	Lease    assignment.LeaseID
	Kind     ObjectKind
	Role     string
	Attempt  assignment.AttemptID
	Target   assignment.TargetID
	Tier     assignment.TierID
}

// Labels renders the ownership as the labels an object carries. The
// values are a compatibility surface between releases: a controller
// sweeps the objects an older one stamped, so a key or a value that
// changes is a sweep that stops finding them. This package's own test
// pins them exactly.
func (o Ownership) Labels() map[string]string {
	labels := map[string]string{
		labelManaged:  "true",
		labelInstance: string(o.Instance),
	}
	for key, value := range map[string]string{
		labelKind:    string(o.Kind),
		labelLease:   string(o.Lease),
		labelRole:    o.Role,
		labelAttempt: string(o.Attempt),
		labelTarget:  string(o.Target),
		labelTier:    string(o.Tier),
	} {
		if value != "" {
			labels[key] = value
		}
	}
	return labels
}

// Mount attaches one named volume into a container at an absolute path,
// whole. Binds and subpaths are deliberately not expressible: every
// mount Runpool makes is a daemon-side volume, which is the same object
// however the controller itself is deployed.
type Mount struct {
	Volume   string
	Target   string
	ReadOnly bool
}

// ContainerSpec is the container shape used by capsules, gateways, and
// short-lived operational probes.
type ContainerSpec struct {
	Name       string
	Image      string
	Entrypoint []string
	Cmd        []string
	Env        []string
	User       string // "" = image default; "0:0" for root setup tasks
	Labels     map[string]string
	Privileged bool
	// NetworkMode overrides Network for daemon-defined modes such as host.
	// Network attaches to a user-defined network by name or id.
	NetworkMode string
	Network     string
	// DNS pins the container's resolvers — the capsule points at its
	// gateway's forwarder, never at anything it could choose itself.
	DNS []netip.Addr
	// CapAdd grants capabilities to an otherwise unprivileged container.
	// The gateway receives NET_ADMIN for its own fail-closed ruleset.
	CapAdd   []string
	GroupAdd []string
	Mounts   []Mount
	// Tmpfs mounts by target path, e.g. "/run/runpool": "rw,size=64k".
	Tmpfs map[string]string

	// Resource limits, the tier envelope applied to the container.
	MemoryBytes     int64
	MemorySwapBytes int64
	NanoCPUs        int64
	PIDsLimit       int64
	// CgroupParent places this container under a shared parent cgroup.
	// A lease's capsule and its gateway name the same parent, so the
	// kernel accounts them as one aggregate and one path reports the
	// lease's whole consumption.
	CgroupParent string
}

// ContainerStatus is the daemon's own word for where a container is in
// its lifecycle.
//
// It is a named type because three other vocabularies in this tree spell
// some of the same words and mean different things by them: a supervisor
// protocol state, an execution observation and an attempt state each have
// a "running". The one that arrives from outside this process is the one
// that must not be mistaken for the others, and it is the one that used
// to be a bare string.
//
// The set below is not closed. A daemon may answer with something this
// build has no name for, and the reader that decides whether a job may
// have run treats an unknown status as proving nothing rather than as
// impossible.
type ContainerStatus string

const (
	StatusCreated    ContainerStatus = "created"
	StatusRunning    ContainerStatus = "running"
	StatusPaused     ContainerStatus = "paused"
	StatusRestarting ContainerStatus = "restarting"
	StatusExited     ContainerStatus = "exited"
	StatusDead       ContainerStatus = "dead"
)

// ContainerState is what the daemon can still say about a container after
// it has stopped. ExitCode is carried alongside Status because a stopped
// container's own filesystem is no longer reachable — no exec, and its
// tmpfs control surface is gone — so the exit code is the only account of
// itself a capsule can leave behind.
type ContainerState struct {
	Status   ContainerStatus
	ExitCode int
}

// OwnedContainer is one labeled container found during reconciliation.
type OwnedContainer struct {
	ID      string
	Name    string
	Kind    ObjectKind
	Role    string
	LeaseID assignment.LeaseID
	Running bool
}

// NetworkSpec describes a user-defined bridge. Isolated is Engine 28's
// bridge gateway mode: combined with Internal, the bridge holds no host
// address at all, so a container on it cannot reach host services even
// by the bridge IP — the property the capsule's sandbox network needs
// and an ordinary internal bridge does not give.
type NetworkSpec struct {
	Name     string
	Internal bool
	Isolated bool
	Labels   map[string]string
}

// OwnedResource is a labeled network or volume found during
// reconciliation. Networks are keyed by id, volumes by name. Role says
// what the object is for — a persistent cache lane carries no lease
// label, and a sweep that reads its empty lease as "orphan" would
// delete every warm cache it finds.
type OwnedResource struct {
	ID      string
	LeaseID assignment.LeaseID
	Role    string
}

// ShortID trims an object id to the width daemon tooling displays,
// tolerating an id shorter than that width. Test fixtures use short
// ids, and so would any future id format — and an unguarded slice
// panics inside an error path, which is the least welcome place for
// one: the message that would have said what went wrong is replaced by
// a crash.
func ShortID(id string) string {
	const width = 12
	if len(id) > width {
		return id[:width]
	}
	return id
}

// VolumeUsage is one owned volume's measured size, as the daemon
// reports it through the disk-usage endpoint.
type VolumeUsage struct {
	Name string
	// Role is the ownership role the volume carries, lifted out of the
	// labels so a caller can ask what a volume is for without knowing
	// how ownership is written down. Labels stays for the vocabulary
	// that is the caller's own, such as which lane a cache volume is.
	Role   string
	Labels map[string]string
	// Size in bytes; -1 when the daemon could not compute it.
	Size int64
}

// FilesystemFree is the free space and free inodes of the daemon's
// storage filesystem, probed from inside it: a helper container's root
// is overlay on the daemon's data root, so statfs there answers for the
// filesystem capsules actually fill — which the controller's own mount
// namespace cannot see when it runs containerized. FreeInodes is -1
// when the filesystem does not account inodes (btrfs reports zero
// totals, which must not read as an empty filesystem).
type FilesystemFree struct {
	FreeBytes  int64
	FreeInodes int64
}

// HostInfo is what the doctor needs to know about the daemon and its
// kernel: enough to decide whether this host can honour the capsule
// contract, and nothing more.
type HostInfo struct {
	ServerVersion string
	APIVersion    string
	Architecture  string
	OSType        string
	CgroupVersion string
	CgroupDriver  string
	MemoryLimit   bool
	SwapLimit     bool
	PidsLimit     bool
	Rootless      bool
	// Physical capacity as the daemon sees it, for the doctor's
	// tiers-versus-host arithmetic.
	NCPU           int
	MemTotalBytes  int64
	SwapTotalBytes int64
	SwapTotalKnown bool
	Warnings       []string
}

// The daemon's answers, in Runpool's vocabulary. A caller that has to
// act differently on one of these gets a sentinel to test for; a caller
// that only reports the failure gets the daemon's own text, which says
// more than a category would.
//
// There are three because three call sites branch. A category nothing
// branches on is a category that will be wrong the first time somebody
// relies on it, since nothing exercises the mapping.
var (
	// ErrNotFound is absence, which is different from unreachability:
	// an execution that cannot be found ended, and one that cannot be
	// asked about is undecided.
	ErrNotFound = errors.New("the daemon has no such object")
	// ErrAlreadyExists is the name being taken. Recovery resolves it by
	// proving ownership; anything else that fails a create is a failure
	// to create.
	ErrAlreadyExists = errors.New("an object of that name already exists")
	// ErrUnavailable is the daemon not answering at all.
	ErrUnavailable = errors.New("the daemon could not be reached")
)

// ErrForeignResource reports an object that carries the expected name
// but not this owner's labels. Adopting it would mean running work
// through — and later deleting — something another instance or a human
// created, so the only safe answer is to stop.
var ErrForeignResource = errors.New("an object with the expected name exists but is not owned by this lease")

// HelperInFlight reports whether this is a container the instance runs
// for itself and has not finished with — the filesystem probe, host-CIDR
// discovery. A sweep has to leave those alone while they run.
//
// The lease id decides ownership, the same rule
// OwnedResource.InstanceInfrastructure states for networks and volumes.
// The second condition is what makes containers different: a lease-less
// network or volume is persistent, while a helper is ephemeral and
// RunTask removes its own on a deadline of its own. So one still running
// is in flight, and force removing it fails the measurement that started
// it; one that is stopped outlived the process that owned it, and
// collecting that is what a sweep is for. Both conditions live here
// rather than at the call sites because a sweep that tests only the
// first deletes the instance's own working parts.
func (c OwnedContainer) HelperInFlight() bool { return c.LeaseID == "" && c.Running }

// InstanceInfrastructure reports whether an owned object belongs to the
// instance itself rather than to a lease — the uplink network, a cache
// lane volume.
//
// The lease id is the whole test. Every object a capsule creates carries
// its lease in the same label map as the rest of its identity, so there
// is no window in which one exists without it; the instance's own
// objects are created without that label on purpose. Naming the
// persistent roles instead only looked equivalent: there are two of them
// — the uplink network and a cache lane volume — and a third added later
// would be swept away as garbage by everything that spelled the rule
// that way.
func (r OwnedResource) InstanceInfrastructure() bool { return r.LeaseID == "" }

// OwnershipFrom reads the ownership an object carries. The second result
// is whether it is Runpool's at all: an object without the managed label
// belongs to somebody else, and nothing about the rest of its labels
// changes that.
func OwnershipFrom(labels map[string]string) (Ownership, bool) {
	if labels[labelManaged] != "true" {
		return Ownership{}, false
	}
	return Ownership{
		Instance: assignment.InstanceID(labels[labelInstance]),
		Lease:    assignment.LeaseID(labels[labelLease]),
		Kind:     ObjectKind(labels[labelKind]),
		Role:     labels[labelRole],
		Attempt:  assignment.AttemptID(labels[labelAttempt]),
		Target:   assignment.TargetID(labels[labelTarget]),
		Tier:     assignment.TierID(labels[labelTier]),
	}, true
}

// ManagedBy is the label pair that selects one instance's objects, for an
// adapter to turn into whatever its engine calls a filter. It is the
// narrowest possible query: everything else about an object is proven by
// reading it back, never by trusting the query.
func ManagedBy(instanceID assignment.InstanceID) map[string]string {
	return map[string]string{labelManaged: "true", labelInstance: string(instanceID)}
}
