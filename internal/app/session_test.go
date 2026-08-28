package app

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rhobuild/runpool/internal/githubactions"

	"github.com/rhobuild/runpool/internal/assignment"

	"github.com/rhobuild/runpool/internal/store"
)

// stubSession is a message session a test can break on purpose. The
// upstream client refreshes an expired session by itself, so the failure
// this stands in for is the one after that refresh has already failed:
// a handle that answers every poll the same way, forever.
type stubSession struct {
	mu       sync.Mutex
	receives int
	closes   int

	// fail is returned from every Receive. When block is set, Receive
	// waits for cancellation instead — the shape of a session that works.
	// offerErr answers every poll with a message the session could not
	// turn into work: the poll succeeded, the acquisition did not.
	fail     error
	block    bool
	offerErr error
	// ackErr is returned from every Acknowledge: the broker took the
	// message and the confirmation could not be delivered, which is the
	// uncertain outcome.
	ackErr error

	// backlog is what this session reports it opened holding.
	backlog *githubactions.Statistics

	// polled closes on the first Receive. Waiting on it is what proves
	// the binding is holding this session and not merely that a
	// replacement was built.
	polled chan struct{}
	once   sync.Once
}

func (s *stubSession) Receive(ctx context.Context) (*githubactions.Message, error) {
	s.mu.Lock()
	s.receives++
	s.mu.Unlock()
	if s.polled != nil {
		s.once.Do(func() { close(s.polled) })
	}
	if s.offerErr != nil {
		// A fresh id per poll, as the broker sends: reusing one makes the
		// delivery a repeat the store already knows, which returns before
		// the session is touched and hides everything downstream of it.
		s.mu.Lock()
		id := s.receives
		s.mu.Unlock()
		return &githubactions.Message{ID: id, AcquireError: s.offerErr}, nil
	}
	if s.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return nil, s.fail
}

func (s *stubSession) Acknowledge(context.Context, int) error { return s.ackErr }
func (s *stubSession) SetCapacity(int)                        {}
func (s *stubSession) Initial() *githubactions.Statistics     { return s.backlog }

func (s *stubSession) Close(context.Context) error {
	s.mu.Lock()
	s.closes++
	s.mu.Unlock()
	return nil
}

func (s *stubSession) counts() (receives, closes int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.receives, s.closes
}

// TestARepeatedlyFailingSessionIsReplaced is the recovery a dead session
// needs. A refresh the upstream client could not complete leaves the
// handle unusable, and every later poll fails identically — so retrying
// the same handle is not recovery, it is a binding that has stopped
// working while the process reports itself healthy.
func TestARepeatedlyFailingSessionIsReplaced(t *testing.T) {
	h := newHarness(t, 1)
	h.srv.pollBackoff = time.Millisecond

	broken := &stubSession{fail: errors.New("session id does not exist")}
	replacement := &stubSession{block: true, polled: make(chan struct{})}

	h.bind.session = broken
	h.bind.newSession = func(context.Context) (providerSession, error) {
		return replacement, nil
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.srv.loop(ctx, h.bind)
	}()

	// Waiting for the replacement to be polled, not merely built: the
	// binding has recovered only once it is the one being asked.
	select {
	case <-replacement.polled:
	case <-time.After(10 * time.Second):
		cancel()
		<-done
		receives, _ := broken.counts()
		t.Fatalf("the binding still polled the dead session after %d failures; a dead "+
			"handle retried forever is a binding that stopped serving in silence", receives)
	}

	cancel()
	<-done

	receives, closes := broken.counts()
	if receives < receiveFailuresBeforeReopen {
		t.Errorf("the session was replaced after %d failures; want at least %d, or a "+
			"single transient failure costs a session", receives, receiveFailuresBeforeReopen)
	}
	if closes != 1 {
		t.Errorf("the failing session was closed %d times; want exactly 1 — a session the "+
			"broker still holds is one the next start has to wait out", closes)
	}
}

// TestASessionThatRecoversIsNotReplaced keeps the previous test from
// passing for the wrong reason: a transient failure must cost a retry,
// not a session.
//
// It runs three failure runs, each one short of the threshold, separated
// by a poll that succeeds - once empty, once carrying a message. Those
// are the two shapes a healthy cycle takes and each has its own reset, so
// dropping either one lets a later run reach the threshold and replace a
// session that was only ever unlucky.
func TestASessionThatRecoversIsNotReplaced(t *testing.T) {
	h := newHarness(t, 1)
	h.srv.pollBackoff = time.Millisecond

	const shortOfThreshold = receiveFailuresBeforeReopen - 1
	if shortOfThreshold < 1 {
		t.Fatalf("receiveFailuresBeforeReopen is %d; a run shorter than the threshold "+
			"needs at least one failure to be a run at all", receiveFailuresBeforeReopen)
	}
	flaky := &intermittentSession{
		failuresPerRun: shortOfThreshold,
		settled:        make(chan struct{}),
	}
	h.bind.session = flaky
	h.bind.newSession = func(context.Context) (providerSession, error) {
		t.Error("a session that recovered between runs was replaced anyway")
		return &stubSession{block: true}, nil
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.srv.loop(ctx, h.bind)
	}()
	select {
	case <-flaky.settled:
	case <-time.After(10 * time.Second):
		t.Error("the flaky session never got through both failure runs")
	}
	cancel()
	<-done
}

// intermittentSession fails in runs, recovering between them. Each
// recovery is a different shape of healthy cycle: an empty long poll,
// then a message that carries nothing to acquire.
type intermittentSession struct {
	mu             sync.Mutex
	seen           int
	failuresPerRun int

	// settled closes once every run and both recoveries are behind it -
	// the point at which a counter that missed either reset would have
	// replaced this session.
	settled chan struct{}
	once    sync.Once
}

func (s *intermittentSession) Receive(ctx context.Context) (*githubactions.Message, error) {
	s.mu.Lock()
	s.seen++
	n, perRun := s.seen, s.failuresPerRun
	s.mu.Unlock()

	switch {
	case n <= perRun:
		return nil, errors.New("transient")
	case n == perRun+1:
		return nil, nil // the empty long poll
	case n <= 2*perRun+1:
		return nil, errors.New("transient")
	case n == 2*perRun+2:
		return &githubactions.Message{ID: n}, nil // a message, nothing to acquire
	case n <= 3*perRun+2:
		return nil, errors.New("transient")
	}
	s.once.Do(func() { close(s.settled) })
	<-ctx.Done()
	return nil, ctx.Err()
}

func (s *intermittentSession) Acknowledge(context.Context, int) error { return nil }
func (s *intermittentSession) SetCapacity(int)                        {}
func (s *intermittentSession) Initial() *githubactions.Statistics     { return nil }
func (s *intermittentSession) Close(context.Context) error            { return nil }

// TestAFailedReopenDoesNotKeepTheDeadSession. Recovery can fail: the
// broker is unreachable, or still holds the session that was just closed.
// What must not survive that is the handle itself. Left in place it is
// polled and announced through for another full run of failures, and
// shutdown closes it a second time and reports a broker holding a session
// nobody holds — which is how a false alarm is manufactured out of an
// orderly stop.
func TestAFailedReopenDoesNotKeepTheDeadSession(t *testing.T) {
	h := newHarness(t, 1)
	h.srv.pollBackoff = time.Millisecond

	dead := &stubSession{fail: errors.New("session id does not exist")}
	replacement := &stubSession{block: true, polled: make(chan struct{})}

	var attempts int
	h.bind.session = dead
	h.bind.newSession = func(context.Context) (providerSession, error) {
		attempts++
		if attempts < 3 {
			return nil, errors.New("the broker still holds this session")
		}
		return replacement, nil
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.srv.loop(ctx, h.bind)
	}()

	select {
	case <-replacement.polled:
	case <-time.After(10 * time.Second):
		cancel()
		<-done
		t.Fatal("the binding never recovered across repeated reopen failures")
	}
	cancel()
	<-done

	receives, closes := dead.counts()
	if receives != receiveFailuresBeforeReopen {
		t.Errorf("the dead session was polled %d times; want exactly %d — after the run "+
			"that gave it up, nothing may reach it again", receives, receiveFailuresBeforeReopen)
	}
	if closes != 1 {
		t.Errorf("the dead session was closed %d times; want exactly 1, or shutdown "+
			"reports a broker holding a session nobody holds", closes)
	}
	if attempts != 3 {
		t.Errorf("the binding made %d open attempts; want 3 — a failed open must be "+
			"retried by the loop, not counted as recovery", attempts)
	}
}

// TestASessionThatCannotAcquireIsReplaced. A poll and an acquisition go
// through the same handle and the same token refresh, so a session whose
// refresh has failed can keep answering polls while failing every
// acquisition. Counted only as a log line, that binding collects offers
// it can never turn into work for as long as the process runs.
func TestASessionThatCannotAcquireIsReplaced(t *testing.T) {
	h := newHarness(t, 1)
	h.srv.pollBackoff = time.Millisecond

	cannotAcquire := &stubSession{offerErr: errors.New("acquire 1 offered jobs: 401")}
	replacement := &stubSession{block: true, polled: make(chan struct{})}

	h.bind.session = cannotAcquire
	h.bind.newSession = func(context.Context) (providerSession, error) {
		return replacement, nil
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.srv.loop(ctx, h.bind)
	}()

	select {
	case <-replacement.polled:
	case <-time.After(10 * time.Second):
		cancel()
		<-done
		receives, _ := cannotAcquire.counts()
		t.Fatalf("the binding kept the session that failed %d acquisitions; a handle that "+
			"can never acquire is one that has stopped serving", receives)
	}
	cancel()
	<-done
}

// TestAReopenedSessionPublishesItsBacklog. Serve feeds the first
// session's assigned backlog to the allocator before any loop runs, so a
// binding with waiting work is not outbid by an idle one. A replacement
// carries its own backlog and must do the same: left at the dead
// session's last figure, a binding recorded at zero demand is one the
// free credit is never shared with, and the queue it is holding never
// drains.
func TestAReopenedSessionPublishesItsBacklog(t *testing.T) {
	h := newHarness(t, 4)
	h.srv.pollBackoff = time.Millisecond

	// A second binding in the same pool, holding demand of its own. The
	// credit is water-filled by demand, so the recovered binding only
	// out-advertises this one if its replacement's backlog was published;
	// at zero demand it is left with the discovery credit, which this
	// one's demand of 1 matches.
	const rival = "other/standard"
	if err := h.srv.alloc.Register(assignment.TierID(h.bind.tier.ID), rival, 4); err != nil {
		t.Fatal(err)
	}
	h.srv.alloc.SetAssignedDemand(rival, 1)
	h.srv.alloc.SetAssignedDemand(h.bind.key, 0)

	dead := &stubSession{fail: errors.New("session id does not exist")}
	replacement := &stubSession{
		block:   true,
		polled:  make(chan struct{}),
		backlog: &githubactions.Statistics{Assigned: 3},
	}

	h.bind.session = dead
	h.bind.newSession = func(context.Context) (providerSession, error) {
		return replacement, nil
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.srv.loop(ctx, h.bind)
	}()
	select {
	case <-replacement.polled:
	case <-time.After(10 * time.Second):
		cancel()
		<-done
		t.Fatal("the binding never recovered")
	}
	cancel()
	<-done

	recovered, other := h.srv.alloc.Advertised(h.bind.key), h.srv.alloc.Advertised(rival)
	if recovered <= other {
		t.Errorf("the recovered binding advertises %d against a rival's %d; its "+
			"replacement session's assigned backlog never reached the allocator",
			recovered, other)
	}
}

// TestAReopenConflictWaitsAtItsOwnInterval. The broker holding this
// binding's previous session is the expected shape of the reopen path -
// the close that gave it up fails there by design - so the loop must
// recognise the conflict and wait at the conflict's interval rather than
// hammering the generic retry. The generic backoff is set prohibitively
// high: if the conflict branch stops being taken, recovery blows the
// test's deadline instead of passing by accident.
func TestAReopenConflictWaitsAtItsOwnInterval(t *testing.T) {
	h := newHarness(t, 1)
	h.srv.pollBackoff = time.Hour
	h.srv.conflictBackoff = time.Millisecond

	replacement := &stubSession{block: true, polled: make(chan struct{})}

	// No session at all: the loop's first act is the open, which is the
	// state a discarded session leaves behind.
	var attempts int
	h.bind.newSession = func(context.Context) (providerSession, error) {
		attempts++
		if attempts < 3 {
			return nil, errors.New(`the session "abc" already exists, status="409 Conflict"`)
		}
		return replacement, nil
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.srv.loop(ctx, h.bind)
	}()
	select {
	case <-replacement.polled:
	case <-time.After(10 * time.Second):
		cancel()
		<-done
		t.Fatal("the binding never recovered through the conflict; the reopen is not " +
			"recognising a 409 and is waiting out the generic backoff instead")
	}
	cancel()
	<-done
}

// TestASessionThatWillNotClearReportsSomethingElse: the report is one
// string per binding, so waiting a conflict out and being stuck behind
// one have to read differently in it.
//
// A conflict is the ordinary shape of a restart, and while the wait is
// ordinary nothing is recorded at all — the binding is not failing to
// reach its provider, and saying so would put every restart in the
// report. Past the point a session expires by inactivity it is not
// ordinary any more, and that is what the record has to carry: an
// operator reading `runpool status` decides whether to act, and the log
// level is not where they look.
//
// Both halves are asserted, and each is gated on the loop's own attempt
// count rather than on the clock, so neither can pass by sampling at a
// convenient moment.
func TestASessionThatWillNotClearReportsSomethingElse(t *testing.T) {
	read := func(t *testing.T, h *harness) string {
		t.Helper()
		var reason string
		if err := h.srv.store.Tx(t.Context(), func(tx *store.Tx) error {
			bindings, err := tx.Bindings()
			for _, b := range bindings {
				if b.ID == h.bind.bindingID {
					reason = b.Contact.LastError
				}
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return reason
	}

	// conflicting runs the loop against a broker that always answers 409,
	// and returns once it has answered n times.
	conflicting := func(t *testing.T, h *harness, grace time.Duration, n int64) {
		t.Helper()
		h.srv.pollBackoff = time.Hour
		h.srv.conflictBackoff = time.Millisecond
		h.srv.conflictGrace = grace

		var attempts atomic.Int64
		reached := make(chan struct{})
		var once sync.Once
		h.bind.newSession = func(context.Context) (providerSession, error) {
			if attempts.Add(1) >= n {
				once.Do(func() { close(reached) })
			}
			return nil, errors.New(`the session "abc" already exists, status="409 Conflict"`)
		}

		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			h.srv.loop(ctx, h.bind)
		}()
		select {
		case <-reached:
		case <-time.After(10 * time.Second):
			cancel()
			<-done
			t.Fatalf("the loop answered fewer than %d conflicts", n)
		}
		cancel()
		<-done
	}

	t.Run("while the wait is ordinary", func(t *testing.T) {
		h := newHarness(t, 1)
		// An hour of grace, so every one of these conflicts is inside it.
		conflicting(t, h, time.Hour, 5)
		if got := read(t, h); got != "" {
			t.Errorf("five conflicts inside the grace recorded %q. Waiting one out is the "+
				"ordinary shape of a restart, and an operator reading the report cannot tell "+
				"it from a session that will not clear.", got)
		}
	})

	t.Run("once it is not", func(t *testing.T) {
		h := newHarness(t, 1)
		conflicting(t, h, 20*time.Millisecond, 200)
		got := read(t, h)
		if !strings.Contains(got, "not clearing on its own") {
			t.Fatalf("the report says %q past the grace; it never distinguished a session that "+
				"will not clear from one being waited out", got)
		}
		if !strings.Contains(got, "already queued") {
			t.Errorf("the reason is %q; it has to say the binding is still serving what it "+
				"has, because an operator reading it decides whether to act", got)
		}
	})
}

// TestAnUnrelatedOutageDoesNotAgeTheConflict: the run of conflicts is a
// run of conflicts.
//
// Whether a session is clearing is judged from how long this binding has
// been conflicting. A failure that is not a conflict says nothing about
// that, and counting the time it lasted would have a binding report a
// session that will not clear before it has waited out a single one.
func TestAnUnrelatedOutageDoesNotAgeTheConflict(t *testing.T) {
	h := newHarness(t, 1)
	h.srv.pollBackoff = time.Millisecond
	h.srv.conflictBackoff = time.Millisecond
	h.srv.conflictGrace = time.Hour

	// One conflict starts the run; an unrelated failure has to end it.
	h.bind.conflictSince = time.Now().Add(-2 * time.Hour)
	h.bind.newSession = func(context.Context) (providerSession, error) {
		return nil, errors.New("the broker is unreachable")
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.srv.loop(ctx, h.bind)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	if !h.bind.conflictSince.IsZero() {
		t.Error("a failure that was not a conflict left the run of conflicts standing, so the " +
			"next conflict is judged against time this binding spent not conflicting")
	}
}
