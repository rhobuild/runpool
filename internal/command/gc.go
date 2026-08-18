package command

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/rhobuild/runpool/internal/cache"
	"github.com/rhobuild/runpool/internal/config"
	"github.com/rhobuild/runpool/internal/platform/docker"
	"github.com/rhobuild/runpool/internal/store"
)

// runGC is the operator's collection pass: plan first, always; touch
// nothing without --apply. The dry run opens the store read-only so it
// can inspect a live controller; apply takes the maintenance lock,
// because evicting under a serving controller's feet is what the
// controller's own monitor is for.
func runGC(streams IO, apply, aggressive bool) error {
	// The policy comes from the configuration when there is one, and
	// from the documented defaults otherwise — a gc run must not invent
	// thresholds the serving controller would not use.
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	if cfg == nil {
		cfg = &config.Config{}
		config.ApplyDefaults(cfg)
	}
	opts := cache.GCOptions{
		TTL: time.Duration(cfg.Cache.Defaults.UnusedTTL),
		TargetBytes: cache.GCTarget(int64(cfg.Cache.Global.MaxManagedBytes),
			cfg.Cache.Global.LowWatermarkPercent),
		AllFree: aggressive,
		Now:     time.Now(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	var st *store.Store
	var dock *docker.Client
	if apply {
		var unlock func()
		st, dock, unlock, err = openStoreAndDocker(ctx)
		if err != nil {
			return err
		}
		defer unlock()
	} else {
		st, err = store.OpenReadOnly(stateDir())
		if errors.Is(err, store.ErrNoState) {
			fmt.Fprintf(streams.Out, "no state in %s: this instance has not run yet\n", stateDir())
			return nil
		}
		if err != nil {
			return err
		}
		dock, err = docker.New(ctx)
		if err != nil {
			return fmt.Errorf("docker: %w", err)
		}
	}
	defer st.Close()
	defer dock.Close()

	mgr := cache.New(st, dock, st.InstanceID())
	plan, err := mgr.PlanGC(ctx, opts)
	if err != nil {
		return err
	}

	fmt.Fprintf(streams.Out, "managed cache: %s across lanes; plan keeps %s\n",
		config.ByteSize(plan.ManagedBytes), config.ByteSize(plan.KeptBytes))
	verb := "would evict"
	if apply {
		verb = "evicting"
	}
	if len(plan.Evictions) == 0 {
		fmt.Fprintln(streams.Out, "nothing to evict")
	}
	for _, e := range plan.Evictions {
		if e.Orphan {
			fmt.Fprintf(streams.Out, "  %s %-34s %8s  orphan volume (no lane row)\n", verb, e.Volume, config.ByteSize(e.Bytes))
			continue
		}
		fmt.Fprintf(streams.Out, "  %s %-34s %8s  %s  %s (%s)\n", verb, e.Volume,
			config.ByteSize(e.Bytes), e.Reason, e.SourceProjectKey, e.Generation)
	}

	// Lease history is the second thing this pass collects. It is reported
	// separately because lanes and rows are different objects: one is disk
	// an operator can see, the other is the books.
	if err := reportLeaseHistory(ctx, streams, st, cfg, apply); err != nil {
		return err
	}

	if !apply {
		fmt.Fprintln(streams.Out, "\ndry run; re-run with --apply to collect")
		return nil
	}
	if len(plan.Evictions) == 0 {
		return nil
	}

	res, err := mgr.RunGC(ctx, plan, "cli")
	if err != nil {
		return err
	}
	fmt.Fprintf(streams.Out, "evicted %d, skipped %d (leased since planning), failed %d\n",
		res.Applied, res.Skipped, len(res.Failed))
	for _, ferr := range res.Failed {
		fmt.Fprintln(streams.Err, " ", ferr)
	}
	// An audit row that could not be written is a hole in the trail, not a
	// failed collection: the lane is already gone, so no later pass can
	// retry it and reporting it as a failure would be false twice over.
	for _, aerr := range res.AuditFailed {
		fmt.Fprintln(streams.Err, "  evicted but not recorded in the audit log:", aerr)
	}
	if len(res.Failed) > 0 {
		return fmt.Errorf("%d eviction(s) failed; the next pass retries them", len(res.Failed))
	}
	return nil
}

// reportLeaseHistory counts, and on --apply forgets, the record of leases
// finished longer ago than the retention window. A zero window keeps
// every one, and says nothing.
//
// The delete runs here rather than through the cache manager because it
// shares nothing with lane eviction: no daemon, no objects to enumerate,
// no per-item outcome. Only the plan-then-apply shape is borrowed.
func reportLeaseHistory(ctx context.Context, streams IO, st *store.Store, cfg *config.Config, apply bool) error {
	window := cfg.Retention.Window()
	if window <= 0 {
		fmt.Fprintln(streams.Out, "lease history: kept indefinitely (retention.leaseHistory is 0)")
		return nil
	}

	const perRun = 5000
	before := time.Now().Add(-window)
	if !apply {
		var n int
		if err := st.Tx(ctx, func(tx *store.Tx) error {
			var err error
			n, err = tx.CountPrunableLeases(before, perRun)
			return err
		}); err != nil {
			return err
		}
		fmt.Fprintf(streams.Out, "lease history: would forget %d record(s) finished before %s",
			n, rfc3339(before))
		// The count is bounded by the same per-run limit the apply uses, so
		// reaching it means "at least this many", not "this many". Printed
		// unqualified it reads as a total, and an operator who ran the apply
		// and saw the same figure would conclude the backlog was cleared.
		if n == perRun {
			fmt.Fprintf(streams.Out, " (this run's limit; more remain)")
		}
		fmt.Fprintln(streams.Out)
		return nil
	}

	var pruned int
	if err := st.Tx(ctx, func(tx *store.Tx) error {
		var err error
		pruned, err = tx.PruneLeaseHistory(before, perRun)
		if err != nil || pruned == 0 {
			return err
		}
		return tx.RecordAudit("cli", "retention_prune", "capsule_leases",
			fmt.Sprintf("pruned=%d older_than=%s", pruned, window))
	}); err != nil {
		return err
	}
	fmt.Fprintf(streams.Out, "lease history: forgot %d record(s)", pruned)
	if pruned == perRun {
		fmt.Fprintf(streams.Out, " (this run's limit; run again to continue)")
	}
	fmt.Fprintln(streams.Out)
	return nil
}
