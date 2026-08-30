package command

import (
	"time"

	"github.com/rhobuild/runpool/internal/assignment"
	"github.com/rhobuild/runpool/internal/config"
	"github.com/rhobuild/runpool/internal/engine"
	"github.com/rhobuild/runpool/internal/store"
)

// The status document is a reporting contract, not a mirror of the
// database: persistence rows change with migrations, while this shape
// changes only with its version string. Field names are snake_case,
// collections are always arrays — an empty one is `[]`, never null,
// because consumers branch on length, not on presence.
const statusAPIVersion = "v1"

// statusHead is what both forms of the document carry, and the whole of
// the pre-serve one.
//
// It is a type rather than four fields repeated in two places, because a
// map that re-spells the tags lets the two forms drift under a rename
// with nothing to notice. It is separate from the served form's fields
// rather than mixed in with them, because the pre-serve answer has to be
// encodable on its own: encoding the whole document there emitted every
// field of the served form as its zero value — thirteen of them,
// including `discrepancies: null`, which this API defines as "the daemon
// could not be asked".
type statusHead struct {
	APIVersion string `json:"api_version"`
	// Served discriminates v1's two forms: true carries the document
	// below, false is the pre-serve form with only state_dir and detail.
	// A consumer branches on this, not on which fields happen to exist.
	Served bool `json:"served"`
	// StateDir and Detail are the pre-serve form's whole payload.
	StateDir string `json:"state_dir,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

type statusDoc struct {
	statusHead
	Instance      string         `json:"instance"`
	HostTopology  string         `json:"host_topology"`
	SchemaVersion int            `json:"schema_version"`
	Scheduling    *schedulingDTO `json:"scheduling,omitempty"`
	DiskPressure  *pressureDTO   `json:"disk_pressure"`
	// Sandbox is the egress policy's last rediscovery. It is here
	// because a pass that fails closes every gateway on this host to all
	// egress -- the right answer to a policy that cannot be shown to be
	// current, and also every running job losing its network at once. It
	// is absent on an instance that maintains no policy.
	Sandbox  *sandboxDTO  `json:"egress_sandbox"`
	Bindings []bindingDTO `json:"bindings"`
	Leases   []leaseDTO   `json:"leases"`
	// ReleasedTotal is how many finished leases the store holds, which the
	// leases array does not say: that array carries only the most recently
	// finished, so its length is what was reported and not what exists. A
	// consumer measuring the array would understate the history by every
	// job beyond the bound.
	ReleasedTotal int           `json:"released_total"`
	CacheLanes    []laneDTO     `json:"cache_lanes"`
	ManualReview  []attemptView `json:"manual_review"`
	// ManualReviewTotal distinguishes the bounded status summary from the
	// complete queue. NextCursor continues it through `attempts list`.
	ManualReviewTotal      int64          `json:"manual_review_total"`
	ManualReviewNextCursor string         `json:"manual_review_next_cursor,omitempty"`
	Containers             []containerDTO `json:"containers"`
	Networks               []resourceDTO  `json:"networks"`
	Volumes                []resourceDTO  `json:"volumes"`
	// Discrepancies is the books-versus-daemon comparison across every
	// observed object kind. It is null only when the daemon could not
	// be asked, which engine_error then explains: an unreadable daemon
	// must not report as a clean one.
	Discrepancies []string `json:"discrepancies"`
	EngineError   string   `json:"engine_error,omitempty"`
	// CapsuleImageError explains a capsule image this command could not
	// resolve. It is about the shipped default only: a tier naming its
	// own capsule_image reports that one, and a launch would run it. The
	// tiers that name none carry the build's reference instead, so this
	// is what tells a reader those are not the images a launch would
	// run — the alternative, refusing to answer at all, took every other
	// fact in this document down with one unset environment variable.
	CapsuleImageError string `json:"capsule_image_error,omitempty"`
}

type manualReviewSummary struct {
	Attempts   []attemptView
	Total      int64
	NextCursor string
}

type schedulingDTO struct {
	Mode                 string `json:"mode"`
	InstanceParallelism  *int   `json:"instance_parallelism"`
	EffectiveParallelism int    `json:"effective_parallelism"`
	Active               int    `json:"active"`
	Available            int    `json:"available"`
	// Queued is work admitted from the provider and waiting for a lease.
	// Active counts leases, so without this an instance holding a hundred
	// attempts and running one reports the same as an idle one.
	Queued int               `json:"queued"`
	Tiers  []tierCapacityDTO `json:"tiers"`
}

type tierCapacityDTO struct {
	ID          string `json:"id"`
	Parallelism int    `json:"parallelism"`
	Active      int    `json:"active"`
	Available   int    `json:"available"`
	// CapsuleImage is what this tier's jobs actually run in: the image the
	// tier names, or the one this build ships. It is reported because a
	// deployment that replaced it is outside the configuration the release
	// gates observed, and that should be visible rather than inferred from
	// a configuration file the reader may not have.
	CapsuleImage string `json:"capsule_image"`
}

// sandboxDTO reports the last rediscovery pass. LastPassAt is when one
// last completed, successful or not, so a loop that stopped reporting is
// visible as a timestamp that stopped moving -- the same way a disk
// measurement that stopped arriving is.
type sandboxDTO struct {
	LastPassAt string `json:"last_pass_at"`
	Error      string `json:"error,omitempty"`
}

type pressureDTO struct {
	Level        string `json:"level"`
	FreeBytes    int64  `json:"free_bytes"`
	FreeInodes   int64  `json:"free_inodes"`
	ManagedBytes int64  `json:"managed_bytes"`
	MeasuredAt   string `json:"measured_at"`
}

// bindingDTO reports one configured source of work. It is provider
// neutral by design: source_binding_key is the provider's own identity,
// versioned and opaque, and a consumer that needs to parse it is reading
// the wrong document.
type bindingDTO struct {
	TargetID         string `json:"target_id"`
	ProviderKind     string `json:"provider_kind"`
	SourceBindingKey string `json:"source_binding_key"`
	// LastContactAt is when a provider call for this binding last
	// succeeded, and LastError what it cannot do now. Both are reported
	// because neither answers alone: an instance holding no leases is
	// either idle or reaching nothing, and only the pair tells them
	// apart. Empty on a binding that has not run yet.
	LastContactAt string `json:"last_contact_at,omitempty"`
	LastError     string `json:"last_error,omitempty"`
	LastErrorAt   string `json:"last_error_at,omitempty"`
}

// leaseDTO reports one lease's runtime footprint. The attempt fields are
// joined in from the record the lease serves: evidence and the project
// belong to the attempt, and reporting them beside the lease is the
// report's job, not the schema's.
type leaseDTO struct {
	ID          string        `json:"id"`
	State       string        `json:"state"`
	Terminal    bool          `json:"terminal"`
	AttemptID   string        `json:"attempt_id"`
	Project     string        `json:"project,omitempty"`
	RuntimeName string        `json:"runtime_name,omitempty"`
	Evidence    string        `json:"evidence"`
	CreatedAt   string        `json:"created_at"`
	Resources   []resourceDTO `json:"resources"`
}

type laneDTO struct {
	ID               string `json:"id"`
	SourceProjectKey string `json:"source_project_key"`
	Generation       string `json:"generation"`
	LeasedBy         string `json:"leased_by,omitempty"`
	LastUsed         string `json:"last_used"`
}

type containerDTO struct {
	Name    string `json:"name"`
	Role    string `json:"role"`
	LeaseID string `json:"lease_id,omitempty"`
	Running bool   `json:"running"`
}

// resourceDTO reports one owned object — a recorded intent on a lease,
// or a network/volume observed on the daemon.
type resourceDTO struct {
	Kind    string `json:"kind"`
	Role    string `json:"role,omitempty"`
	Name    string `json:"name"`
	LeaseID string `json:"lease_id,omitempty"`
	State   string `json:"state,omitempty"`
}

func statusDocument(snap store.Snapshot, cfg *config.Config, review manualReviewSummary, obs daemonObservation, shippedCapsule string) statusDoc {
	topology := "unknown"
	if cfg != nil {
		topology = string(cfg.Host.Topology)
	}
	doc := statusDoc{
		statusHead:    statusHead{APIVersion: statusAPIVersion, Served: true},
		Instance:      snap.InstanceID,
		HostTopology:  topology,
		SchemaVersion: snap.SchemaVersion,
		Bindings:      []bindingDTO{},
		Leases:        []leaseDTO{},
		ReleasedTotal: snap.ReleasedTotal,
		CacheLanes:    []laneDTO{},
		ManualReview:  []attemptView{},
		Containers:    []containerDTO{},
		Networks:      []resourceDTO{},
		Volumes:       []resourceDTO{},
	}
	if cfg != nil {
		doc.Scheduling = schedulingStatus(cfg, snap.Leases, snap.Queued, shippedCapsule)
	}
	if sb := snap.Sandbox; sb != nil {
		doc.Sandbox = &sandboxDTO{LastPassAt: rfc3339(time.Unix(sb.At, 0)), Error: sb.Error}
	}
	if p := snap.Pressure; p != nil {
		doc.DiskPressure = &pressureDTO{
			Level:        p.Level,
			FreeBytes:    p.FreeBytes,
			FreeInodes:   p.FreeInodes,
			ManagedBytes: p.ManagedBytes,
			MeasuredAt:   rfc3339(time.Unix(p.MeasuredAt, 0)),
		}
	}
	for _, b := range snap.Bindings {
		doc.Bindings = append(doc.Bindings, bindingDTO{
			TargetID: string(b.TargetID), ProviderKind: b.ProviderKind,
			SourceBindingKey: string(b.SourceBindingKey),
			LastContactAt:    rfc3339(b.Contact.LastContact),
			LastError:        b.Contact.LastError,
			LastErrorAt:      rfc3339(b.Contact.LastErrorAt),
		})
	}
	for _, l := range snap.Leases {
		attempt := snap.Attempts[l.ID]
		project := ""
		if attempt.TenantKey != "" || attempt.ProjectKey != "" {
			project = attempt.TenantKey + "/" + attempt.ProjectKey
		}
		lease := leaseDTO{
			ID:          string(l.ID),
			State:       string(l.State),
			Terminal:    l.State.Terminal(),
			AttemptID:   string(l.AttemptID),
			Project:     project,
			RuntimeName: string(l.RuntimeName),
			Evidence:    string(attempt.Evidence),
			CreatedAt:   rfc3339(l.CreatedAt),
			Resources:   []resourceDTO{},
		}
		for _, in := range snap.Resources[l.ID] {
			lease.Resources = append(lease.Resources, resourceDTO{
				Kind: string(in.Kind), Role: in.Role, Name: in.Name, LeaseID: string(in.LeaseID), State: in.State,
			})
		}
		doc.Leases = append(doc.Leases, lease)
	}
	for _, c := range snap.CacheLanes {
		doc.CacheLanes = append(doc.CacheLanes, laneDTO{
			ID: c.ID, SourceProjectKey: c.SourceProjectKey, Generation: c.Generation,
			LeasedBy: string(c.LeasedBy), LastUsed: rfc3339(time.Unix(c.LastUsed, 0)),
		})
	}
	doc.ManualReview = append(doc.ManualReview, review.Attempts...)
	doc.ManualReviewTotal = review.Total
	doc.ManualReviewNextCursor = review.NextCursor
	for _, c := range obs.containers {
		doc.Containers = append(doc.Containers, containerDTO{
			Name: c.Name, Role: string(c.Role), LeaseID: string(c.LeaseID), Running: c.Running,
		})
	}
	for _, n := range obs.networks {
		doc.Networks = append(doc.Networks, resourceDTO{Kind: "network", Role: string(n.Role), Name: n.ID, LeaseID: string(n.LeaseID)})
	}
	for _, v := range obs.volumes {
		doc.Volumes = append(doc.Volumes, resourceDTO{Kind: "volume", Role: string(v.Role), Name: v.ID, LeaseID: string(v.LeaseID)})
	}
	if obs.err != nil {
		doc.EngineError = obs.err.Error()
	} else {
		doc.Discrepancies = discrepancies(snap.Leases, obs)
	}
	return doc
}

func schedulingStatus(cfg *config.Config, leases []store.Lease, queued map[int64]int, shippedCapsule string) *schedulingDTO {
	activeByTier := make(map[assignment.TierID]int, len(cfg.Tiers))
	active := 0
	for _, lease := range leases {
		if lease.State.Terminal() {
			continue
		}
		active++
		activeByTier[lease.TierID]++
	}

	mode := "independent-tiers"
	effective := 0
	if cfg.Scheduling.Parallelism != nil {
		mode = "global"
		effective = *cfg.Scheduling.Parallelism
	} else {
		for _, tier := range cfg.Tiers {
			effective += tier.Parallelism
		}
	}
	dto := &schedulingDTO{
		Mode: mode, InstanceParallelism: cfg.Scheduling.Parallelism,
		EffectiveParallelism: effective, Active: active, Tiers: []tierCapacityDTO{},
	}
	dto.Queued = queuedAttemptCount(queued)
	dto.Available = dto.EffectiveParallelism - active
	if dto.Available < 0 {
		dto.Available = 0
	}
	for _, tier := range cfg.Tiers {
		tierActive := activeByTier[assignment.TierID(tier.ID)]
		available := tier.Parallelism - tierActive
		if available < 0 {
			available = 0
		}
		if cfg.Scheduling.Parallelism != nil && available > dto.Available {
			available = dto.Available
		}
		dto.Tiers = append(dto.Tiers, tierCapacityDTO{
			ID: tier.ID, Parallelism: tier.Parallelism, Active: tierActive, Available: available,
			CapsuleImage: tier.Image(shippedCapsule),
		})
	}
	return dto
}

// daemonObservation is what the daemon reported about this instance's
// objects, or the error that prevented asking.
type daemonObservation struct {
	containers []engine.OwnedContainer
	networks   []engine.OwnedResource
	volumes    []engine.OwnedResource
	err        error
}

// discrepancies compares the books with everything the daemon showed —
// containers, networks and volumes, not containers alone, because a
// leaked network is exactly as much a disagreement as a leaked
// container and used to be invisible here. An empty (non-nil) result
// means the comparison ran and found agreement.
func discrepancies(leases []store.Lease, obs daemonObservation) []string {
	live := map[assignment.LeaseID]bool{}
	for _, l := range leases {
		if !l.State.Terminal() {
			live[l.ID] = true
		}
	}
	out := []string{}

	withContainer := map[assignment.LeaseID]bool{}
	for _, c := range obs.containers {
		// A helper the instance is measuring with belongs to no lease by
		// design, so judging it against the lease set reports a
		// disagreement on every pass that catches one. Stopped is
		// different: that one outlived its process and is a real leak.
		if c.HelperInFlight() {
			continue
		}
		withContainer[c.LeaseID] = true
		if !live[c.LeaseID] {
			out = append(out, "container "+c.Name+" belongs to no live lease")
		}
	}
	for _, l := range leases {
		if live[l.ID] && l.State == store.LeaseWorkloadRunning && !withContainer[l.ID] {
			out = append(out, "lease "+string(l.ID)+" claims to be running with no container")
		}
	}

	check := func(kind string, resources []engine.OwnedResource, persistentRole engine.Role) {
		for _, r := range resources {
			switch {
			case r.Role == persistentRole:
				// Instance infrastructure: a lane or the uplink carries
				// no lease on purpose.
			case r.LeaseID == "":
				out = append(out, kind+" "+r.ID+" carries no lease and no persistent role")
			case !live[r.LeaseID]:
				out = append(out, kind+" "+r.ID+" belongs to no live lease")
			}
		}
	}
	check("network", obs.networks, engine.RoleUplink)
	check("volume", obs.volumes, engine.RoleCacheLane)
	return out
}

// rfc3339 renders a time for the report, and renders a zero time as
// nothing at all: a field that has never been set is absent rather than
// carrying an epoch a consumer would read as a real moment.
func rfc3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
