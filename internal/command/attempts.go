package command

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"time"

	"github.com/rhobuild/runpool/internal/app"
	"github.com/rhobuild/runpool/internal/assignment"
	"github.com/rhobuild/runpool/internal/store"
)

const (
	defaultAttemptPageSize = 50
	maxAttemptCursorLength = 512
)

type attemptListState string

const (
	attemptListManualReview attemptListState = "manual-review"
	attemptListReady        attemptListState = "ready"
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
	// ReviewedAt and SettledAt complete the review record: who resolved
	// it says half, and an operator auditing a decision needs the when.
	// Absent means the event has not happened, and the field is omitted.
	ReviewedAt string `json:"reviewed_at,omitempty"`
	SettledAt  string `json:"settled_at,omitempty"`
	AgeSeconds int64  `json:"age_seconds"`
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

type attemptListDocument struct {
	State      attemptListState `json:"state"`
	Attempts   []attemptView    `json:"attempts"`
	Total      int64            `json:"total"`
	NextCursor string           `json:"next_cursor,omitempty"`
}

// attemptCursorToken is the transport shape of the opaque CLI cursor. The
// state is carried with the position so a manual-review cursor cannot silently
// skip unrelated rows when reused against the ready queue.
type attemptCursorToken struct {
	State      attemptListState `json:"state"`
	ReceivedAt int64            `json:"received_at"`
	AttemptID  string           `json:"attempt_id"`
}

func viewOf(a store.Attempt, now time.Time) attemptView {
	return attemptView{
		ID:           string(a.ID),
		Workload:     string(a.SourceWorkloadKey),
		Project:      string(a.TenantKey) + "/" + string(a.ProjectKey),
		State:        string(a.State),
		ReviewReason: string(a.ReviewReason),
		Resolution:   string(a.Resolution),
		ReviewedBy:   a.ReviewedBy,
		ReviewedAt:   epochRFC3339(a.ReviewedAt),
		SettledAt:    epochRFC3339(a.SettledAt),
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

func runAttemptsList(streams IO, stateFilter string, asJSON bool, pageSize int, encodedCursor string) error {
	state, err := parseAttemptListState(stateFilter)
	if err != nil {
		return err
	}
	if pageSize < 1 || pageSize > store.MaxAttemptPageSize {
		return usagef("--limit must be between 1 and %d", store.MaxAttemptPageSize)
	}
	cursor, err := decodeAttemptCursor(encodedCursor, state)
	if err != nil {
		return usagef("invalid --cursor: %v", err)
	}

	err = inReadOnlyStore(func(s *store.Tx) error {
		page, err := listAttempts(s, state, cursor, pageSize)
		if err != nil {
			return err
		}
		now := time.Now()
		views := make([]attemptView, len(page.Attempts))
		for i, a := range page.Attempts {
			views[i] = viewOf(a, now)
		}
		document := attemptListDocument{State: state, Attempts: views, Total: page.Total}
		if page.Next != nil {
			document.NextCursor, err = encodeAttemptCursor(state, *page.Next)
			if err != nil {
				return err
			}
		}
		if asJSON {
			return json.NewEncoder(streams.Out).Encode(document)
		}
		if len(views) == 0 {
			fmt.Fprintf(streams.Out, "no attempts in %s (total: %d)\n", state, page.Total)
			return nil
		}
		fmt.Fprintf(streams.Out, "%-22s %-28s %-24s %-24s %s\n", "ATTEMPT", "WORKLOAD", "PROJECT", "REASON", "AGE")
		for _, v := range views {
			fmt.Fprintf(streams.Out, "%-22s %-28s %-24s %-24s %s\n",
				v.ID, v.Workload, v.Project, v.ReviewReason,
				age(time.Duration(v.AgeSeconds)*time.Second))
		}
		fmt.Fprintf(streams.Out, "\nshowing %d of %d attempts\n", len(views), page.Total)
		if document.NextCursor != "" {
			fmt.Fprintf(streams.Out, "next page: runpool attempts list --state %s --limit %d --cursor %s\n",
				state, pageSize, document.NextCursor)
		}
		return nil
	})
	if errors.Is(err, store.ErrNoState) {
		// No state is no attempts. An instance that has never run holds
		// nothing for review and nothing ready, and the listing says so
		// in the listing's own shape — whether it has run at all is
		// status's question, answered in status's document.
		if asJSON {
			return json.NewEncoder(streams.Out).Encode(attemptListDocument{
				State: state, Attempts: []attemptView{}, Total: 0,
			})
		}
		fmt.Fprintf(streams.Out, "no attempts in %s (total: 0)\n", state)
		return nil
	}
	return err
}

func parseAttemptListState(value string) (attemptListState, error) {
	state := attemptListState(value)
	switch state {
	case attemptListManualReview, attemptListReady:
		return state, nil
	default:
		return "", usagef("attempts list serves --state manual-review and ready; %q is not wired", value)
	}
}

// listAttempts serves the two states an operator can act on: work held for
// review, and work waiting for admission. Both are globally ordered reporting
// views; scheduling remains binding-scoped.
func listAttempts(s *store.Tx, state attemptListState, cursor *store.AttemptCursor, pageSize int) (store.AttemptPage, error) {
	if state == attemptListManualReview {
		return s.ManualReviewAttemptPage(cursor, pageSize)
	}
	return s.ReadyAttemptPage(cursor, pageSize)
}

func encodeAttemptCursor(state attemptListState, cursor store.AttemptCursor) (string, error) {
	payload, err := json.Marshal(attemptCursorToken{
		State: state, ReceivedAt: cursor.ReceivedAt.Unix(), AttemptID: string(cursor.ID),
	})
	if err != nil {
		return "", fmt.Errorf("encode attempt cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(payload), nil
}

func decodeAttemptCursor(encoded string, state attemptListState) (*store.AttemptCursor, error) {
	if encoded == "" {
		return nil, nil
	}
	if len(encoded) > maxAttemptCursorLength {
		return nil, errors.New("cursor is too long")
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, errors.New("cursor is not valid base64url")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var token attemptCursorToken
	if err := decoder.Decode(&token); err != nil {
		return nil, fmt.Errorf("cursor payload: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("cursor payload contains trailing data")
	}
	if token.State != state {
		return nil, fmt.Errorf("cursor is for state %q, not %q", token.State, state)
	}
	if token.AttemptID == "" {
		return nil, errors.New("cursor attempt id is empty")
	}
	if token.ReceivedAt < 0 {
		return nil, errors.New("cursor time is before the Unix epoch")
	}
	return &store.AttemptCursor{
		ReceivedAt: time.Unix(token.ReceivedAt, 0).UTC(),
		ID:         assignment.AttemptID(token.AttemptID),
	}, nil
}

func runAttemptsInspect(streams IO, id string, asJSON bool) error {
	err := inReadOnlyStore(func(s *store.Tx) error {
		attempt, err := s.Get(assignment.AttemptID(id))
		if err != nil {
			return err
		}
		events, err := s.Events(assignment.AttemptID(id))
		if err != nil {
			return err
		}
		refs, err := s.AttemptProviderReferences(assignment.AttemptID(id))
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
			fmt.Fprintf(streams.Out, "resolved  %s by %s at %s\n", v.Resolution, v.ReviewedBy, v.ReviewedAt)
		}
		if v.SettledAt != "" {
			fmt.Fprintf(streams.Out, "settled   %s\n", v.SettledAt)
		}
		for _, name := range slices.Sorted(maps.Keys(v.Provider)) {
			fmt.Fprintf(streams.Out, "provider  %-18s %s\n", name, v.Provider[name])
		}
		fmt.Fprintln(streams.Out, "lifecycle:")
		for _, ev := range events {
			fmt.Fprintf(streams.Out, "  %s  %-28s %s\n",
				rfc3339(time.Unix(ev.CreatedAt, 0)), ev.Kind, ev.Detail)
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
		// The preview reads the attempt it promises to act on. Printed
		// from the argument alone, a typo'd or already-settled id
		// previewed as a confident action and exited zero, and only
		// --apply discovered the truth.
		if err := inReadOnlyStore(func(tx *store.Tx) error {
			attempt, err := tx.Get(assignment.AttemptID(id))
			if err != nil {
				return fmt.Errorf("attempt %s: %w", id, err)
			}
			if attempt.State != store.AttemptManualReview {
				return fmt.Errorf("attempt %s is %s, not %s; the applying form would refuse it",
					id, attempt.State, store.AttemptManualReview)
			}
			return nil
		}); err != nil {
			return err
		}
		fmt.Fprintf(streams.Out, "would %s attempt %s\n  actor:  %s\n  reason: %s\nre-run with --apply to perform it\n",
			decision, id, actor, reason)
		return nil
	}

	// The writer applies it. Whoever holds the lock owns every write to
	// this state, and while a controller is running that is the
	// controller -- so the decision goes to it rather than the operator
	// taking the lock away, which is what stopping it amounted to. On a
	// shared host stopping the controller is stopping every tenant's CI,
	// for one attempt.
	decided := app.ResolveRequest{Attempt: id, Reason: reason, Actor: actor, Decision: app.DecisionSettle}
	if retry {
		decided.Decision = app.DecisionRetry
	}
	switch _, err := app.ResolveThroughController(stateDir(), decided, nil); {
	case err == nil:
		fmt.Fprintf(streams.Out, "attempt %s: %s (by %s)\n", id, decision, actor)
		return nil
	case !errors.Is(err, app.ErrNoController):
		// It answered, and refused. That is the controller's answer and
		// the operator's to act on, not something to retry another way.
		return err
	}

	// Nothing answered. Either there is no controller, and the lock is
	// free for this write, or there is one that cannot carry a
	// resolution -- which the lock tells apart, because the kernel
	// releases a dead owner's and a stale socket file cannot fake a
	// listener.
	lock, err := store.TryAcquire(stateDir())
	if err != nil {
		return fmt.Errorf("the controller is running but does not answer resolutions "+
			"(is it older than the version that added the maintenance socket?): %w; "+
			"upgrade and restart it, or stop it to resolve directly", err)
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
			return tx.ResolveReviewToReady(assignment.AttemptID(id), reason, actor)
		}
		return tx.ResolveReviewToSettled(assignment.AttemptID(id), assignment.ResolutionMayHaveExecuted, reason, actor)
	}); err != nil {
		return err
	}
	fmt.Fprintf(streams.Out, "attempt %s: %s (by %s)\n", id, decision, actor)
	return nil
}

// epochRFC3339 renders a unix-seconds timestamp, or nothing for the
// zero: these columns are NULL until their event happens, and the zero
// epoch rendered as 1970 would date every unreviewed attempt to it.
func epochRFC3339(sec int64) string {
	if sec == 0 {
		return ""
	}
	return rfc3339(time.Unix(sec, 0))
}
