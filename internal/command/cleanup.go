package command

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/rhobuild/runpool/internal/config"
	"github.com/rhobuild/runpool/internal/credential"
	"github.com/rhobuild/runpool/internal/platform/docker"
	"github.com/rhobuild/runpool/internal/platform/githubactions"
	"github.com/rhobuild/runpool/internal/store"

	"github.com/rhobuild/runpool/internal/assignment"
)

// Destructive commands default to dry-run: the operator must ask for the
// change, and for uninstall must name the instance being destroyed.
// Neither command ever prunes daemon-wide or touches a resource whose
// ownership labels do not name this instance.

func runCleanup(streams IO, apply bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	st, dock, unlock, err := openForMaintenance(ctx, apply)
	if err != nil {
		return err
	}
	defer unlock()
	defer st.Close()
	defer dock.Close()

	plan, err := planOwnedResources(ctx, st, dock)
	if err != nil {
		return err
	}

	// Cleanup removes what no live lease needs: resources belonging to
	// released leases, and objects the books never recorded. A running
	// capsule is left alone — stopping work is what drain is for.
	keep := map[string]bool{}
	snap, err := st.Snapshot()
	if err != nil {
		return err
	}
	for _, l := range snap.Leases {
		if !l.State.Terminal() {
			keep[string(l.ID)] = true
		}
	}
	removable := plan.exclude(keep)

	if removable.empty() {
		fmt.Fprintln(streams.Out, "nothing to clean up")
		return nil
	}
	fmt.Fprint(streams.Out, removable.describe("would remove", apply))
	if !apply {
		fmt.Fprintln(streams.Out, "\nre-run with --apply to remove them")
		return nil
	}
	if err := removable.remove(ctx, dock); err != nil {
		return err
	}
	fmt.Fprintln(streams.Out, "cleanup complete")
	return nil
}

func runUninstall(streams IO, confirm string, deleteScaleSets bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	st, dock, unlock, err := openForMaintenance(ctx, confirm != "")
	if err != nil {
		return err
	}
	defer unlock()
	defer st.Close()
	defer dock.Close()

	snap, err := st.Snapshot()
	if err != nil {
		return err
	}
	plan, err := planOwnedResources(ctx, st, dock)
	if err != nil {
		return err
	}

	// The scale sets are the adapter's, read from its own metadata: the
	// core knows bindings, and only this composition layer turns one
	// back into something GitHub can be asked to delete.
	var ghBindings []store.GitHubBinding
	if deleteScaleSets {
		if err := st.Tx(ctx, func(tx *store.Tx) error {
			var err error
			ghBindings, err = tx.GitHubBindings()
			return err
		}); err != nil {
			return err
		}
	}

	dryRun := confirm == ""
	// Before anything is described in the present tense. A wrong id is
	// refused, and a command that will refuse must not first print the
	// line it would print while removing.
	if !dryRun && confirm != snap.InstanceID {
		return fmt.Errorf("confirmation %q does not name this instance (%s)", confirm, snap.InstanceID)
	}
	fmt.Fprintf(streams.Out, "instance %s\n", snap.InstanceID)
	fmt.Fprint(streams.Out, plan.describe("would remove", !dryRun))
	for _, gb := range ghBindings {
		fmt.Fprintf(streams.Out, "  scale set %s on %s\n", gb.ScaleSetName, gb.CanonicalURL)
	}
	// Every lease row, not the snapshot's bounded view: this number is
	// what an operator weighs before destroying the books.
	fmt.Fprintf(streams.Out, "  %d lease records\n", liveLeaseCount(snap)+snap.ReleasedTotal)
	// Queued attempts have no lease yet, so the count above does not see
	// them. They are work the provider assigned and this instance
	// acknowledged, and purging is what loses it.
	if queued := queuedAttemptCount(snap.Queued); queued > 0 {
		fmt.Fprintf(streams.Out, "  %d attempts waiting for a lease\n", queued)
	}

	if dryRun {
		fmt.Fprintf(streams.Out, "\ndry run. To proceed: runpool uninstall --confirm=%s%s\n",
			snap.InstanceID, map[bool]string{true: " --delete-scale-sets"}[deleteScaleSets])
		return nil
	}

	if err := plan.remove(ctx, dock); err != nil {
		return err
	}
	if len(ghBindings) > 0 {
		if err := deleteConfiguredScaleSets(ctx, streams, ghBindings); err != nil {
			return fmt.Errorf("delete scale sets (local resources are removed; retry uninstall to finish): %w", err)
		}
	}
	// Everything durable goes in one transaction, children before
	// parents: intents, leases, then the delivery and attempt evidence.
	// That evidence is the record of work this instance accepted, and
	// uninstall is the operator saying it goes with the instance.
	if err := st.Tx(ctx, func(tx *store.Tx) error {
		return tx.PurgeEverything()
	}); err != nil {
		return fmt.Errorf("purging durable records: %w", err)
	}
	fmt.Fprintln(streams.Out, "uninstall complete; the deployment-managed state volume remains for the operator to remove")
	return nil
}

// deleteConfiguredScaleSets removes the scale sets this instance created,
// resolving each target's credential from the current configuration.
func deleteConfiguredScaleSets(ctx context.Context, streams IO, bindings []store.GitHubBinding) error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return fmt.Errorf("configuration needed to reach GitHub: %w", err)
	}
	creds := map[string]config.Credential{}
	for _, c := range cfg.Credentials {
		creds[c.ID] = c
	}
	byURL := map[string]config.Target{}
	for _, t := range cfg.Targets {
		if ref, err := config.ParseTargetURL(t.URL); err == nil {
			byURL[ref.CanonicalURL] = t
		}
	}
	for _, gb := range bindings {
		if gb.ScaleSetID == 0 {
			fmt.Fprintf(streams.Out, "  %s: no scale set was recorded\n", gb.ScaleSetName)
			continue
		}
		target, ok := byURL[gb.CanonicalURL]
		if !ok {
			return fmt.Errorf("scale set %s belongs to %s, which is absent from the current configuration", gb.ScaleSetName, gb.CanonicalURL)
		}
		secret, err := credential.Resolve(os.Getenv, creds[target.CredentialID])
		if err != nil {
			return err
		}
		gh, err := githubactions.NewClient(githubactions.ClientConfig{
			ConfigURL: gb.CanonicalURL, Credential: secret, Version: "uninstall",
		})
		if err != nil {
			return err
		}
		remote, found, err := gh.ScaleSetByName(ctx, gb.RunnerGroup, gb.ScaleSetName)
		if err != nil {
			return err
		}
		if !found {
			fmt.Fprintf(streams.Out, "  scale set %s is already absent\n", gb.ScaleSetName)
			continue
		}
		if int64(remote.ID) != gb.ScaleSetID {
			return fmt.Errorf("scale set %s now has id %d, but this instance recorded %d; refusing to delete a different resource",
				gb.ScaleSetName, remote.ID, gb.ScaleSetID)
		}
		if err := gh.DeleteScaleSet(ctx, int(gb.ScaleSetID)); err != nil {
			return fmt.Errorf("delete scale set %s: %w", gb.ScaleSetName, err)
		}
		fmt.Fprintf(streams.Out, "  deleted scale set %s\n", gb.ScaleSetName)
	}
	return nil
}

// ownedPlan is the set of Docker objects this instance owns, grouped so
// removal happens in dependency order.
type ownedPlan struct {
	instanceID string
	containers []docker.OwnedContainer
	networks   []docker.OwnedResource
	volumes    []docker.OwnedResource
}

func planOwnedResources(ctx context.Context, st *store.Store, dock *docker.Client) (ownedPlan, error) {
	p := ownedPlan{instanceID: string(st.InstanceID())}
	var err error
	id := p.instanceID
	if p.containers, err = dock.ListOwnedContainers(ctx, assignment.InstanceID(id)); err != nil {
		return p, err
	}
	if p.networks, err = dock.ListOwnedNetworks(ctx, assignment.InstanceID(id)); err != nil {
		return p, err
	}
	p.volumes, err = dock.ListOwnedVolumes(ctx, assignment.InstanceID(id))
	return p, err
}

func (p ownedPlan) exclude(keep map[string]bool) ownedPlan {
	out := ownedPlan{instanceID: p.instanceID}
	for _, c := range p.containers {
		// A helper the instance is still running is not garbage. Apply
		// holds the singleton lock so it cannot meet one, but the dry
		// run opens read-only to inspect a live controller — and that
		// preview has to name what an apply would take, not what it
		// happened to catch mid-measurement.
		if c.HelperInFlight() {
			continue
		}
		if !keep[c.LeaseID] {
			out.containers = append(out.containers, c)
		}
	}
	for _, n := range p.networks {
		// Instance infrastructure is not lease garbage; uninstall removes
		// it via the full plan.
		if n.InstanceInfrastructure() {
			continue
		}
		if !keep[n.LeaseID] {
			out.networks = append(out.networks, n)
		}
	}
	for _, v := range p.volumes {
		// A cache lane has no lease on purpose: warm cache is the
		// product working, not garbage. Cleanup leaves lanes alone;
		// uninstall removes them because it takes the full plan.
		if v.InstanceInfrastructure() {
			continue
		}
		if !keep[v.LeaseID] {
			out.volumes = append(out.volumes, v)
		}
	}
	return out
}

func (p ownedPlan) empty() bool {
	return len(p.containers)+len(p.networks)+len(p.volumes) == 0
}

func (p ownedPlan) describe(verb string, applying bool) string {
	if applying {
		verb = "removing"
	}
	out := fmt.Sprintf("%s %d containers, %d networks, %d volumes\n",
		verb, len(p.containers), len(p.networks), len(p.volumes))
	for _, c := range p.containers {
		out += fmt.Sprintf("  container %s\n", c.Name)
	}
	for _, n := range p.networks {
		out += fmt.Sprintf("  network %.12s\n", n.ID)
	}
	for _, v := range p.volumes {
		out += fmt.Sprintf("  volume %s\n", v.ID)
	}
	return out
}

// remove deletes containers first, then the networks and volumes they
// held open.
func (p ownedPlan) remove(ctx context.Context, dock *docker.Client) error {
	for _, c := range p.containers {
		if err := dock.RemoveOwnedContainer(ctx, c.ID, assignment.InstanceID(p.instanceID), assignment.LeaseID(c.LeaseID)); err != nil {
			return fmt.Errorf("remove container %s: %w", c.Name, err)
		}
	}
	for _, n := range p.networks {
		if err := dock.RemoveOwnedNetwork(ctx, n.ID, assignment.InstanceID(p.instanceID), assignment.LeaseID(n.LeaseID)); err != nil {
			return fmt.Errorf("remove network %.12s: %w", n.ID, err)
		}
	}
	for _, v := range p.volumes {
		if err := dock.RemoveOwnedVolume(ctx, v.ID, assignment.InstanceID(p.instanceID), assignment.LeaseID(v.LeaseID)); err != nil {
			return fmt.Errorf("remove volume %s: %w", v.ID, err)
		}
	}
	return nil
}

// openStoreAndDocker takes the singleton lock before touching anything
// destructive: a maintenance command that races a live controller can
// delete resources out from under a running job. The lock is released
// when the returned closer runs.
func openStoreAndDocker(ctx context.Context) (*store.Store, *docker.Client, func(), error) {
	lock, err := store.TryAcquire(stateDir())
	if err != nil {
		if errors.Is(err, store.ErrLockHeld) {
			return nil, nil, nil, fmt.Errorf(
				"a controller is running against this state directory; stop it before running maintenance")
		}
		return nil, nil, nil, err
	}
	st, err := store.Open(stateDir(), store.DefaultRetryBudget)
	if err != nil {
		lock.Release()
		return nil, nil, nil, err
	}
	return finishOpen(ctx, st, func() { lock.Release() })
}

// openForMaintenance picks the store a maintenance command may have: a
// dry run only reports, so it opens read-only and takes no lock; an apply
// changes things and takes the singleton lock first.
func openForMaintenance(ctx context.Context, apply bool) (*store.Store, *docker.Client, func(), error) {
	if apply {
		return openStoreAndDocker(ctx)
	}
	return previewStore(ctx)
}

// previewStore opens the store read-only for a dry run, the same rule the
// rest of the CLI follows: reporting must not migrate — or create — a store
// a live controller owns. Going through openStoreAndDocker meant `runpool
// uninstall` with no flags, the preview the help text tells an operator to
// run first, upgraded the schema and wrote a pre-migration backup; run with
// an older binary it refused to preview at all. It also took the exclusive
// lock, so neither preview could inspect a running controller.
func previewStore(ctx context.Context) (*store.Store, *docker.Client, func(), error) {
	st, err := store.OpenReadOnly(stateDir())
	if errors.Is(err, store.ErrSchemaBehind) {
		// The preview reads and the applying form migrates, so a schema
		// the controller has not caught up to closes the safe path while
		// leaving the irreversible one open. Saying so is what keeps an
		// operator from reading the refusal as "this instance is fine".
		return nil, nil, nil, fmt.Errorf("%w\n"+
			"  - a preview reads the state and does not migrate it; the applying form would", err)
	}
	if err != nil {
		return nil, nil, nil, err
	}
	return finishOpen(ctx, st, func() {})
}

func finishOpen(ctx context.Context, st *store.Store, release func()) (*store.Store, *docker.Client, func(), error) {
	dock, err := docker.New(ctx)
	if err != nil {
		st.Close()
		release()
		return nil, nil, nil, err
	}
	return st, dock, release, nil
}

// queuedAttemptCount is the work admitted from the provider that has not
// reached a lease. It is invisible to liveLeaseCount by construction, and
// it is exactly what an idle-looking instance can still be holding.
func queuedAttemptCount(queued map[int64]int) int {
	n := 0
	for _, q := range queued {
		n += q
	}
	return n
}

// liveLeaseCount counts the leases a snapshot carries whole. Released
// ones are bounded, so their total comes from the snapshot's own count
// rather than from the slice.
func liveLeaseCount(snap store.Snapshot) int {
	n := 0
	for _, l := range snap.Leases {
		if !l.State.Terminal() {
			n++
		}
	}
	return n
}
