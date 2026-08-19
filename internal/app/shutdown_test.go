package app

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rhobuild/runpool/internal/platform/githubactions"
	"github.com/rhobuild/runpool/internal/store"

	"github.com/rhobuild/runpool/internal/assignment"
)

// gatedSession reports when its Close begins and then holds until the
// test releases it. What it measures is overlap: with every session
// closing under one budget the starts all arrive before any release,
// and a shutdown that closed one session at a time can only present
// one start at a time.
type gatedSession struct {
	started chan<- string
	release <-chan struct{}
	key     string
}

func (g *gatedSession) Receive(context.Context) (*githubactions.Message, error) { return nil, nil }
func (g *gatedSession) Acknowledge(context.Context, int) error                  { return nil }
func (g *gatedSession) SetCapacity(int)                                         {}
func (g *gatedSession) Initial() *githubactions.Statistics                      { return nil }
func (g *gatedSession) Close(ctx context.Context) error {
	g.started <- g.key
	select {
	case <-g.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// The deployment's stop grace period is sized against drain plus one
// session-close budget, so the closes have to overlap: a shutdown that
// closed sessions one after another would owe the platform a bound that
// grows with the binding count, and no grace period an operator chose
// would keep holding it.
func TestSessionsCloseTogether(t *testing.T) {
	started := make(chan string, 3)
	release := make(chan struct{})
	defer close(release)

	s := &Controller{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for _, key := range []assignment.BindingKey{"a", "b", "c"} {
		s.bindings = append(s.bindings, &binding{
			key:     key,
			session: &gatedSession{started: started, release: release, key: string(key)},
		})
	}
	// A binding whose loop never opened a session has nothing to close;
	// it must be skipped, not dereferenced.
	s.bindings = append(s.bindings, &binding{key: "never-opened"})

	done := make(chan struct{})
	go func() { s.closeSessions(); close(done) }()

	for i := 0; i < 3; i++ {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatalf("after %d of 3 closes began no further close arrived; "+
				"sessions are closing one at a time", i)
		}
	}
	// All three are in flight at once; releasing them ends the shutdown.
	select {
	case <-done:
		t.Fatal("closeSessions returned while sessions were still closing")
	default:
	}
}

// countingRemover is the daemon's removal surface, succeeding and
// counting: the abandonment test needs to see zero removals, and its
// mutation half needs to see some.
type countingRemover struct{ calls atomic.Int64 }

func (c *countingRemover) RemoveOwnedContainer(context.Context, string, assignment.InstanceID, assignment.LeaseID) error {
	c.calls.Add(1)
	return nil
}
func (c *countingRemover) RemoveOwnedNetwork(context.Context, string, assignment.InstanceID, assignment.LeaseID) error {
	c.calls.Add(1)
	return nil
}
func (c *countingRemover) RemoveOwnedVolume(context.Context, string, assignment.InstanceID, assignment.LeaseID) error {
	c.calls.Add(1)
	return nil
}

// Once the drain window has elapsed the successor's adoption owns
// recovery, and the failures still arriving in this process are the
// shutdown itself — the client and store closing under live waits.
// Recovery acting on them would rewrite a running lease to cleaning and
// hand the next start a failure to dismantle instead of a capsule to
// adopt, so with the abandonment marked it must leave the lease exactly
// as it is.
func TestDrainExpiryAbandonsRecovery(t *testing.T) {
	h := newHarness(t, 1)
	remover := &countingRemover{}
	h.useRemover(remover)
	if err := h.deliver(demand("job-abandoned", "app", 41)); err != nil {
		t.Fatal(err)
	}
	lease, _ := leaseFor(t, h, "job-abandoned")
	if err := h.store.Tx(t.Context(), func(tx *store.Tx) error {
		id, err := tx.PlanResource(lease.ID, store.ResourceContainer, "runner", "runpool-runner-abandoned")
		if err != nil {
			return err
		}
		return tx.MarkResourcePresent(id, "abandoned-1")
	}); err != nil {
		t.Fatal(err)
	}
	before := reloadLease(t, h, lease.ID).State

	h.srv.abandoning.Store(true)
	if err := h.srv.recoverCapsuleFailure(t.Context(), h.bind, lease.ID, ""); err != nil {
		t.Fatalf("abandoned recovery reported an error: %v", err)
	}
	if got := reloadLease(t, h, lease.ID).State; got != before {
		t.Fatalf("lease moved from %s to %s during abandonment; it must be left as it is", before, got)
	}
	if n := remover.calls.Load(); n != 0 {
		t.Fatalf("abandonment removed %d objects; it must dismantle nothing", n)
	}

	// The mutation half: the same call without the abandonment is the
	// destructive path, so the assertions above are proven able to fail.
	h.srv.abandoning.Store(false)
	if err := h.srv.recoverCapsuleFailure(t.Context(), h.bind, lease.ID, ""); err != nil {
		t.Fatalf("live recovery failed: %v", err)
	}
	if got := reloadLease(t, h, lease.ID).State; got == before {
		t.Fatalf("live recovery left the lease in %s; the test cannot observe the destructive path", got)
	}
	if remover.calls.Load() == 0 {
		t.Fatal("live recovery removed nothing; the test cannot observe the destructive path")
	}
}

// The flag is raised by the drain window elapsing and by nothing else: a
// clean drain hands the sessions a quiet shutdown, and recovery keeps
// acting until the window is genuinely spent.
func TestDrainWindowExpiryMarksAbandonment(t *testing.T) {
	h := newHarness(t, 1)
	h.srv.drainWindow = 30 * time.Millisecond
	h.srv.wg.Add(1) // a capsule goroutine that will not finish in time
	defer h.srv.wg.Done()
	if err := h.srv.drain(); err != nil {
		t.Fatal(err)
	}
	if !h.srv.abandoning.Load() {
		t.Fatal("the drain window elapsed and abandonment was not marked")
	}

	clean := newHarness(t, 1)
	clean.srv.drainWindow = time.Second
	if err := clean.srv.drain(); err != nil {
		t.Fatal(err)
	}
	if clean.srv.abandoning.Load() {
		t.Fatal("a clean drain marked abandonment")
	}
}
