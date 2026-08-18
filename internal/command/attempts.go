package command

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"time"

	"github.com/rhobuild/runpool/internal/assignment"
	"github.com/rhobuild/runpool/internal/store"
)

// attemptView is the reporting DTO: reporting rows are not persistence
// rows, and this one adds the derived age an operator triages by.
type attemptView struct {
	ID           string `json:"id"`
	Workload     string `json:"workload"`
	Project      string `json:"project"`
	State        string `json:"state"`
	ReviewReason string `json:"review_reason,omitempty"`
	Resolution   string `json:"resolution,omitempty"`
	ReviewedBy   string `json:"reviewed_by,omitempty"`
	AgeSeconds   int64  `json:"age_seconds"`
	// Evidence is the furthest this attempt's execution was ever shown to
	// have got. It is what a resolution turns on — retrying is safe only
	// where the work provably never began — so a report that withheld it
	// left the decision to be made without the fact it depends on.
	// Reported by inspect; list is a triage view.
	Evidence string `json:"evidence,omitempty"`
	// Provider carries the provider's own identifiers, by name, for the
	// external check resolving a held attempt requires. Empty when the
	// provider recorded none.
	Provider map[string]string `json:"provider,omitempty"`
}

func viewOf(a store.Attempt, now time.Time) attemptView {
	return attemptView{
		ID:           a.ID,
		Workload:     a.SourceWorkloadKey,
		Project:      a.TenantKey + "/" + a.ProjectKey,
		State:        a.State,
		ReviewReason: a.ReviewReason,
		Resolution:   a.Resolution,
		ReviewedBy:   a.ReviewedBy,
		AgeSeconds:   int64(now.Sub(time.Unix(a.ReceivedAt, 0)).Seconds()),
	}
}

// inReadOnlyStore runs fn against the read-only store: listing and
// inspecting must not migrate — or create — a database a live
// controller owns. ErrNoState propagates: what an absent state means is
// the caller's question — an empty listing for list, a failure for
// inspect — and answering it here forced every caller into one shape.
func inReadOnlyStore(fn func(*store.Tx) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := store.OpenReadOnly(stateDir())
	if err != nil {
		return err
	}
	defer st.Close()
	return st.Tx(ctx, fn)
}

func runAttemptsList(streams IO, stateFilter string, asJSON bool) error {
	if stateFilter != "manual-review" && stateFilter != "ready" {
		// Capability-accurate: no silent empty answers for filters this
		// command does not actually serve.
		return usagef("attempts list serves --state manual-review and ready; %q is not wired", stateFilter)
	}

	err := inReadOnlyStore(func(s *store.Tx) error {
		attempts, err := listAttempts(s, stateFilter)
		if err != nil {
			return err
		}
		now := time.Now()
		views := make([]attemptView, len(attempts))
		for i, a := range attempts {
			views[i] = viewOf(a, now)
		}
		if asJSON {
			return json.NewEncoder(streams.Out).Encode(views)
		}
		if len(views) == 0 {
			fmt.Fprintf(streams.Out, "no attempts in %s\n", stateFilter)
			return nil
		}
		fmt.Fprintf(streams.Out, "%-22s %-28s %-24s %-24s %s\n", "ATTEMPT", "WORKLOAD", "PROJECT", "REASON", "AGE")
		for _, v := range views {
			fmt.Fprintf(streams.Out, "%-22s %-28s %-24s %-24s %s\n",
				v.ID, v.Workload, v.Project, v.ReviewReason,
				(time.Duration(v.AgeSeconds) * time.Second).String())
		}
		return nil
	})
	if errors.Is(err, store.ErrNoState) {
		// No state is no attempts. An instance that has never run holds
		// nothing for review and nothing ready, and the listing says so
		// in the listing's own shape — whether it has run at all is
		// status's question, answered in status's document.
		if asJSON {
			fmt.Fprintln(streams.Out, "[]")
			return nil
		}
		fmt.Fprintf(streams.Out, "no attempts in %s\n", stateFilter)
		return nil
	}
	return err
}

// listAttempts serves the two states an operator can act on: work held
// for review, and work waiting for admission. Ready attempts are gathered
// per binding because that is how the queue is keyed, and because an
// operator asking why nothing runs wants to know which binding is holding
// the backlog.
func listAttempts(s *store.Tx, stateFilter string) ([]store.Attempt, error) {
	if stateFilter == "manual-review" {
		return s.ManualReviewAttempts()
	}
	bindings, err := s.Bindings()
	if err != nil {
		return nil, err
	}
	var out []store.Attempt
	for _, b := range bindings {
		ready, err := s.ReadyAttempts(b.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, ready...)
	}
	return out, nil
}

func runAttemptsInspect(streams IO, id string, asJSON bool) error {
	err := inReadOnlyStore(func(s *store.Tx) error {
		attempt, err := s.Get(id)
		if err != nil {
			return err
		}
		events, err := s.Events(id)
		if err != nil {
			return err
		}
		refs, err := s.AttemptProviderReferences(id)
		if err != nil {
			return err
		}
		v := viewOf(attempt, time.Now())
		v.Evidence = string(attempt.Evidence)
		v.Provider = refs
		if asJSON {
			return json.NewEncoder(streams.Out).Encode(map[string]any{
				"attempt": v,
				"events":  events,
			})
		}
		fmt.Fprintf(streams.Out, "attempt   %s\nworkload  %s\nproject   %s\nstate     %s\n",
			v.ID, v.Workload, v.Project, v.State)
		if v.Evidence != "" {
			fmt.Fprintf(streams.Out, "evidence  %s\n", v.Evidence)
		}
		if v.ReviewReason != "" {
			fmt.Fprintf(streams.Out, "held      %s\n", v.ReviewReason)
		}
		if v.Resolution != "" {
			fmt.Fprintf(streams.Out, "resolved  %s by %s\n", v.Resolution, v.ReviewedBy)
		}
		for _, name := range slices.Sorted(maps.Keys(v.Provider)) {
			fmt.Fprintf(streams.Out, "provider  %-18s %s\n", name, v.Provider[name])
		}
		fmt.Fprintln(streams.Out, "lifecycle:")
		for _, ev := range events {
			fmt.Fprintf(streams.Out, "  %s  %-28s %s\n",
				time.Unix(ev.CreatedAt, 0).UTC().Format(time.RFC3339), ev.Kind, ev.Detail)
		}
		return nil
	})
	if errors.Is(err, store.ErrNoState) {
		// Inspect names one attempt, and with no state that attempt does
		// not exist: a failure that says why, not an empty success.
		return fmt.Errorf("attempt %s: no state in %s; this instance has not run yet", id, stateDir())
	}
	return err
}

// runAttemptsResolve is the operator deciding held work. Exactly one of
// retry and settle is a decision; both or neither is a person who has
// not decided, and guessing which they meant is how a job runs twice.
func runAttemptsResolve(streams IO, id string, retry, settle bool, reason, actor string, apply bool) error {
	if retry == settle {
		return usagef("choose exactly one of --retry and --settle-may-have-run")
	}
	if reason == "" {
		return usagef("--reason is required: every resolution is audited")
	}
	if actor == "" {
		actor = os.Getenv("USER")
	}
	if actor == "" {
		return usagef("--actor is required: every resolution names its actor, and $USER is empty")
	}

	decision := "settle as may-have-run"
	if retry {
		decision = "retry"
	}
	if !apply {
		fmt.Fprintf(streams.Out, "would %s attempt %s\n  actor:  %s\n  reason: %s\nre-run with --apply to perform it\n",
			decision, id, actor, reason)
		return nil
	}

	// The resolve mutates live scheduling state, so it takes the same
	// singleton lock the controller holds: a resolution racing a live
	// controller could requeue an attempt the controller is disposing.
	lock, err := store.TryAcquire(stateDir())
	if err != nil {
		return fmt.Errorf("cannot take the maintenance lock: %w; stop the controller before resolving attempts", err)
	}
	defer lock.Release()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := store.Open(stateDir(), store.DefaultRetryBudget)
	if err != nil {
		return err
	}
	defer st.Close()

	if err := st.Tx(ctx, func(tx *store.Tx) error {
		if retry {
			return tx.ResolveReviewToReady(id, reason, actor)
		}
		return tx.ResolveReviewToSettled(id, assignment.ResolutionMayHaveExecuted, reason, actor)
	}); err != nil {
		return err
	}
	fmt.Fprintf(streams.Out, "attempt %s: %s (by %s)\n", id, decision, actor)
	return nil
}
