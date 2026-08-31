package command

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/rhobuild/runpool/internal/app"
	"github.com/rhobuild/runpool/internal/config"
	"github.com/rhobuild/runpool/internal/engine/docker"
	"github.com/rhobuild/runpool/internal/store"

	"github.com/rhobuild/runpool/internal/assignment"
)

const statusManualReviewPageSize = 50

// runStatus answers the operability questions the design requires an
// operator to answer without reading SQLite: what this instance owns,
// which leases are live, which cache lanes are held, and whether the
// books agree with the daemon.
// runStatus takes the image this build ships rather than the image a
// launch would run: resolving that is an observation like the daemon
// read and the configuration read below, and it is made here so a
// failure is reported rather than answered with nothing.
func runStatus(streams IO, asJSON bool, buildCapsule string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Read-only: reporting must not migrate — or create — a store a live
	// controller owns.
	st, err := store.OpenReadOnly(stateDir())
	if errors.Is(err, store.ErrNoState) {
		// A report that answers in prose is still a report, and --json
		// promised a document. The first scripted call after an install
		// meets exactly this state, so answering it with a line of text
		// and exit 0 is a parse failure that looks like success.
		return reportNoState(streams, asJSON)
	}
	if err != nil {
		return err
	}
	defer st.Close()

	snap, err := st.Snapshot()
	if err != nil {
		return err
	}

	// The held-work queue rides along: an operator reading status must
	// see what is waiting for a person without knowing to ask.
	review := manualReviewSummary{}
	if err := st.Tx(ctx, func(tx *store.Tx) error {
		page, err := tx.ManualReviewAttemptPage(nil, statusManualReviewPageSize)
		if err != nil {
			return err
		}
		review.Total = page.Total
		now := time.Now()
		for _, a := range page.Attempts {
			review.Attempts = append(review.Attempts, viewOf(a, now))
		}
		if page.Next != nil {
			review.NextCursor, err = encodeAttemptCursor(attemptListManualReview, *page.Next)
		}
		return err
	}); err != nil {
		return err
	}

	// Docker observation is best-effort: status must still report the
	// books when the daemon is unreachable, which is itself a finding.
	// Containers, networks and volumes are all observed — agreement
	// judged from containers alone once hid every leaked network.
	var obs daemonObservation
	if dock, err := docker.New(ctx); err == nil {
		defer dock.Close()
		if obs.containers, err = dock.ListOwnedContainers(ctx, assignment.InstanceID(snap.InstanceID)); err == nil {
			if obs.networks, err = dock.ListOwnedNetworks(ctx, assignment.InstanceID(snap.InstanceID)); err == nil {
				obs.volumes, err = dock.ListOwnedVolumes(ctx, assignment.InstanceID(snap.InstanceID))
			}
		}
		obs.err = err
	} else {
		obs.err = err
	}

	cfg := configuredStatusConfig(os.Getenv)
	// Resolved the way serve resolves it, and best-effort like every
	// other observation here: an environment this cannot resolve is a
	// finding to report, not a reason to answer nothing. A --json caller
	// that got a bare error and no document had no way to tell an
	// unresolvable image from an unreachable host.
	//
	// It resolves from the environment of whoever ran this command,
	// which in the reference deployment is the controller's own, so the
	// answer is labelled with the failure rather than presented as the
	// controller's view.
	shippedCapsule, imageErr := app.CapsuleImage(os.Getenv, buildCapsule)
	if imageErr != nil {
		// CapsuleImage returns the empty string beside its error, and
		// CapsuleImageError promises the tier entries carry what the
		// build ships. Without this every tier reported an empty image,
		// which reads as "none configured" rather than "this one could
		// not be resolved" — and those call for different actions.
		//
		// buildCapsule is never empty here: an empty one is substituted
		// for the development default, which resolves without error, so
		// reaching this line at all means the build stamped a reference.
		shippedCapsule = buildCapsule
	}
	doc := statusDocument(snap, cfg, review, obs, shippedCapsule)
	if imageErr != nil {
		doc.CapsuleImageError = imageErr.Error()
	}
	if asJSON {
		enc := json.NewEncoder(streams.Out)
		enc.SetIndent("", "  ")
		return enc.Encode(doc)
	}

	fmt.Fprintf(streams.Out, "instance %s (schema v%d, %s)\n", snap.InstanceID, snap.SchemaVersion, doc.HostTopology)
	if capacity := doc.Scheduling; capacity != nil {
		fmt.Fprintf(streams.Out, "scheduling: %s, effective parallelism %d, %d active, %d available, %d queued\n",
			capacity.Mode, capacity.EffectiveParallelism, capacity.Active, capacity.Available, capacity.Queued)
		for _, tier := range capacity.Tiers {
			fmt.Fprintf(streams.Out, "  %-16s parallelism %-4d active %-4d available %d\n",
				tier.ID, tier.Parallelism, tier.Active, tier.Available)
		}
	}

	if doc.CapsuleImageError != "" {
		// Said in the text form too: the tier lines above carry what
		// this build ships, and without this a reader takes them for
		// the images a launch would run.
		fmt.Fprintf(streams.Out, "capsule image: unresolved (%s)\n", doc.CapsuleImageError)
	}

	if sb := snap.Sandbox; sb != nil {
		// Said in the text form too, and this one most of all: a
		// rediscovery that failed closed every gateway on this host to
		// all egress, which is every running job losing its network at
		// once. An operator who types `runpool status` and is told
		// nothing has been told the wrong thing.
		if sb.Error != "" {
			fmt.Fprintf(streams.Out, "\negress policy: every gateway closed to all egress %s (%s)\n",
				ago(time.Now(), sb.At), sb.Error)
			fmt.Fprintln(streams.Out, "  see docs/runbook.md for what closed them and what to do")
		} else {
			fmt.Fprintf(streams.Out, "\negress policy: in force, rechecked %s\n",
				ago(time.Now(), sb.At))
		}
	}

	if p := snap.Pressure; p != nil {
		if p.Level == "unknown" {
			fmt.Fprintf(streams.Out, "\ndisk pressure: unknown (measurement unavailable, checked %s)\n",
				ago(time.Now(), p.MeasuredAt))
		} else {
			fmt.Fprintf(streams.Out, "\ndisk pressure: %s (free %s, managed cache %s, measured %s)\n",
				p.Level, config.ByteSize(p.FreeBytes), config.ByteSize(p.ManagedBytes),
				ago(time.Now(), p.MeasuredAt))
		}
		if p.Level != "normal" {
			fmt.Fprintln(streams.Out, "  see docs/runbook.md for what this level means and what to do")
		}
	}

	fmt.Fprintf(streams.Out, "\nbindings (%d)\n", len(snap.Bindings))
	now := time.Now()
	for _, b := range snap.Bindings {
		fmt.Fprintf(streams.Out, "  %-14s %-16s %s\n",
			b.TargetID, b.ProviderKind, b.ConfiguredBindingKey)
		fmt.Fprintf(streams.Out, "  %-14s %s\n", "", providerReach(b.Contact, now))
	}

	// The released figure is the store's total, not what the snapshot
	// carries: the snapshot bounds finished history, and a count that
	// silently meant "the recent ones" would understate what is there.
	live, released := splitLeases(snap.Leases)
	fmt.Fprintf(streams.Out, "\nleases: %d live, %d released", len(live), snap.ReleasedTotal)
	if snap.ReleasedTotal > len(released) {
		fmt.Fprintf(streams.Out, " (showing the %d most recent)", len(released))
	}
	fmt.Fprintln(streams.Out)
	renderLeases(streams.Out, live, snap)

	if review.Total > 0 {
		fmt.Fprintf(streams.Out, "\nmanual review (%d total; showing %d) — resolve with `runpool attempts resolve`\n",
			review.Total, len(review.Attempts))
		for _, v := range review.Attempts {
			fmt.Fprintf(streams.Out, "  %-22s %-28s %-24s held %s for %s\n",
				v.ID, v.Workload, v.Project, v.ReviewReason,
				(time.Duration(v.AgeSeconds) * time.Second).String())
		}
		if review.NextCursor != "" {
			fmt.Fprintf(streams.Out, "  continue: runpool attempts list --state manual-review --cursor %s\n",
				review.NextCursor)
		}
	}

	fmt.Fprintf(streams.Out, "\ncache lanes (%d)\n", len(snap.CacheLanes))
	for _, c := range snap.CacheLanes {
		holder := "free"
		if c.LeasedBy != "" {
			holder = "leased by " + string(c.LeasedBy)
		}
		fmt.Fprintf(streams.Out, "  %-16s %-12s %-40s %s\n", c.ID, c.Generation, c.SourceProjectKey, holder)
	}

	fmt.Fprintf(streams.Out, "\nowned containers (%d)\n", len(obs.containers))
	for _, c := range obs.containers {
		running := "exited"
		if c.Running {
			running = "running"
		}
		fmt.Fprintf(streams.Out, "  %-30s %-8s %-8s lease %s\n", c.Name, c.Role, running, c.LeaseID)
	}
	fmt.Fprintf(streams.Out, "\nowned networks (%d), volumes (%d)\n", len(obs.networks), len(obs.volumes))
	if obs.err != nil {
		fmt.Fprintf(streams.Out, "\nengine: %v — the books could not be compared with the daemon\n", obs.err)
		return nil
	}

	// The disagreements that matter, across every observed object kind.
	// Reconciliation fixes them at startup; surfacing them here explains
	// a degraded instance.
	if len(doc.Discrepancies) > 0 {
		fmt.Fprintf(streams.Out, "\ndiscrepancies\n")
		for _, d := range doc.Discrepancies {
			fmt.Fprintf(streams.Out, "  %s\n", d)
		}
	}
	return nil
}

// configuredStatusConfig is best-effort because status remains useful when
// the active configuration is temporarily invalid or credentials are absent.
// File mode can be decoded without resolving credential values. Quick Start
// reconstructs only the fields needed by reporting.
func configuredStatusConfig(environ func(string) string) *config.Config {
	if path := environ(config.EnvConfigFile); path != "" {
		if cfg, err := config.LoadFile(path); err == nil {
			return cfg
		}
		return nil
	}
	topology := environ(config.EnvHostTopology)
	if topology == "" {
		return nil
	}
	parallelism := 1
	if raw := environ(config.EnvParallelism); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 {
			return nil
		}
		parallelism = parsed
	}
	tierID := environ(config.EnvTier)
	if tierID == "" {
		tierID = config.DefaultTierID
	}
	return &config.Config{
		Host:  config.Host{Topology: config.HostTopology(topology)},
		Tiers: []config.Tier{{ID: tierID, Parallelism: parallelism}},
	}
}

// renderLeases writes the live leases, and for a quarantined one the
// objects it still holds.
//
// Named for that state and no other, because there the objects are the
// work: a quarantined lease is stuck on an object rather than on a
// decision, and clearing it means finding that object. A count says how
// many there are to look for, which is not the same as saying which, and
// the runbook sent an operator here for exactly that. They are not in
// `discrepancies` either -- that list is objects belonging to no live
// lease, and a quarantined lease is live.
//
// Every lease listing its own would bury the report a busy host prints.
func renderLeases(w io.Writer, live []store.Lease, snap store.Snapshot) {
	for _, l := range live {
		resources := snap.Resources[l.ID]
		project := ""
		if a, ok := snap.Attempts[l.ID]; ok {
			project = string(a.TenantKey) + "/" + string(a.ProjectKey)
		}
		fmt.Fprintf(w, "  %-16s %-18s %-28s %d resources\n", l.ID, l.State, project, len(resources))
		if l.State != store.LeaseQuarantined {
			continue
		}
		for _, r := range resources {
			fmt.Fprintf(w, "      %-10s %-30s %s\n", r.Kind, r.Name, r.State)
		}
	}
}

func splitLeases(all []store.Lease) (live, released []store.Lease) {
	for _, l := range all {
		if l.State.Terminal() {
			released = append(released, l)
		} else {
			live = append(live, l)
		}
	}
	return live, released
}

// providerReach is the one line that separates an idle instance from one
// reaching nothing. Both hold no leases and answer every local query, so
// without this the most likely first-run failure — a binding whose
// provider never sends it work — looks exactly like success.
func providerReach(c store.ProviderContact, now time.Time) string {
	switch {
	case c.LastContact.IsZero() && c.LastError == "":
		return "provider: no contact recorded; this binding has not served yet"
	// Not strictly after: the two moments are recorded by the same loop
	// pass and a failure that lands in the same millisecond as the success
	// before it is still the later fact. A tie goes to the failure,
	// because a report that hides one is worse than one that shows a
	// stale one.
	case c.LastError != "" && !c.LastContact.After(c.LastErrorAt):
		since := "never reached"
		if !c.LastContact.IsZero() {
			since = "last reached " + ago(now, c.LastContact)
		}
		return fmt.Sprintf("provider: failing since %s (%s): %s",
			ago(now, c.LastErrorAt), since, c.LastError)
	default:
		return "provider: last reached " + ago(now, c.LastContact)
	}
}

// ago renders a gap the way an operator reads one: the size of the wait,
// not a timestamp to subtract from the current time by hand.
func ago(now, then time.Time) string { return age(now.Sub(then)) + " ago" }

// age renders a span the way an operator reads one: the size of the
// wait, not a timestamp to subtract by hand.
//
// A negative span reads as zero. Two clocks disagree, and a record
// written a moment ahead of the reader is ordinary rather than
// remarkable — but "-1m0s ago" reads as a bug in the thing being
// diagnosed, which is the last place to send someone looking.
func age(d time.Duration) string {
	d = d.Round(time.Second)
	if d < 0 {
		d = 0
	}
	return d.String()
}

// reportNoState answers the one state every command can meet before the
// controller has ever run, in whichever form the caller asked for.
func reportNoState(streams IO, asJSON bool) error {
	if asJSON {
		// The head alone. Encoding the whole document here emitted the
		// served form's thirteen other fields as their zero values, and
		// a reader following the published shape would have taken
		// `discrepancies: null` to mean the daemon could not be asked.
		return json.NewEncoder(streams.Out).Encode(statusHead{
			APIVersion: statusAPIVersion,
			Served:     false,
			StateDir:   stateDir(),
			Detail:     "this instance has not run yet",
		})
	}
	fmt.Fprintf(streams.Out, "no state in %s: this instance has not run yet\n", stateDir())
	return nil
}
