package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rhobuild/runpool/internal/assignment"
	"github.com/rhobuild/runpool/internal/platform/githubactions"
	"github.com/rhobuild/runpool/internal/store"
)

// loop is one binding's demand cycle: announce the allocator's credit,
// translate what the broker committed, and queue it. Messages are
// hints; the state store carries the truth.
//
// The announcement is a whole number of credits, which may be zero. A
// binding announcing zero is silent, not blind: the tier's rotating
// discovery credit reaches it within one pass of the pool.
func (s *Controller) loop(ctx context.Context, b *binding) {
	failures := 0
	for {
		if ctx.Err() != nil {
			return
		}
		// The scale set is ensured before anything reads its id.
		//
		// A provider that cannot be reached here leaves the binding where
		// an unreachable broker leaves it: retrying, while the capsules it
		// already owns run to completion. What must not happen is serving
		// past it. Draining a ready attempt is not local work — it mints a
		// JIT credential against this scale set — so an attempt served
		// before the id is confirmed is served against an id read from the
		// store, which is proof of ownership and not proof that the set
		// still exists. Each such attempt fails and spends one of its
		// servings, so a provider outage at startup would hold a whole
		// queue for review rather than waiting the outage out.
		//
		// Ensuring first is also what makes the id safe to publish: it is
		// written here, on this goroutine, before any capsule goroutine
		// that reads it exists.
		if !b.ensured {
			if err := s.ensureScaleSet(ctx, b); err == nil {
				s.recordProviderContact(ctx, b)
			} else {
				if ctx.Err() != nil {
					return
				}
				s.log.Error("cannot create or adopt the scale set; the binding keeps retrying",
					"binding", b.key, "name", b.scaleSetName, "error", err)
				s.recordProviderFailure(ctx, b, err)
				select {
				case <-time.After(s.backoff()):
				case <-ctx.Done():
					return
				}
				continue
			}
		}

		// Local work drains whether or not the broker is reachable, so
		// this runs before the session is required — and after the scale
		// set, which it needs.
		s.scheduleReadyAttempts(ctx, b)

		if b.session == nil {
			if err := s.openSession(ctx, b); err == nil {
				b.conflictSince = time.Time{}
				s.recordProviderContact(ctx, b)
			} else {
				if ctx.Err() != nil {
					return
				}
				// A conflict here is this binding's own previous session,
				// which the broker holds until it expires by inactivity -
				// the close that gave it up is expected to fail. It is
				// the ordinary shape of this path, so it waits at the
				// conflict's own interval and does not report an error
				// indistinguishable from a revoked credential.
				wait := s.backoff()
				if githubactions.IsSessionConflict(err) {
					wait = sessionConflictBackoff
					if s.conflictBackoff != 0 {
						wait = s.conflictBackoff
					}
					// Ordinary on a restart, so it is not reported as a
					// failure to reach the provider — the provider is
					// answering. It stops being ordinary once it outlasts
					// the inactivity the broker expires a session on, and
					// past that the binding serves nothing for a reason
					// nothing else would carry.
					b.conflictSince = firstOf(b.conflictSince, time.Now())
					if held := time.Since(b.conflictSince); held > s.sessionGrace() {
						s.log.Error("the broker has held this binding's session past the point it expires by inactivity",
							"binding", b.key, "held", held.Round(time.Second))
						// A different reason, not the same 409 the wait
						// has been recording. The report is one string
						// per binding, so an operator reading it sees
						// what the log level already said only if the
						// two states say different things: waiting one
						// out is ordinary, and a session that outlasts
						// what a session can outlast is not. It carries
						// no elapsed time on purpose -- the record is
						// written when the reason changes, and a reason
						// that changes every pass would be written every
						// pass.
						s.recordProviderFailure(ctx, b, fmt.Errorf(
							"the broker has held this binding's session past the point one expires "+
								"by inactivity, so it is not clearing on its own; only work already "+
								"queued is being served: %w", err))
					} else {
						s.log.Info("the broker still holds this binding's previous session; waiting for it to expire",
							"binding", b.key)
					}
				} else {
					// Not a conflict, so the run of conflicts is over
					// whatever else is wrong. Left standing, an outage
					// between two conflicts counts toward the run, and a
					// binding that has waited out nothing reports a
					// session that will not clear.
					b.conflictSince = time.Time{}
					s.log.Error("cannot open a message session; the binding keeps retrying",
						"binding", b.key, "error", err)
					s.recordProviderFailure(ctx, b, err)
				}
				select {
				case <-time.After(wait):
				case <-ctx.Done():
					return
				}
				continue
			}
		}
		s.announce(b)

		msg, err := b.session.Receive(ctx)
		if err == nil {
			s.recordProviderContact(ctx, b)
		} else {
			if ctx.Err() != nil {
				return
			}
			failures++
			s.log.Error("receive failed; retrying", "binding", b.key,
				"consecutive_failures", failures, "error", err)
			s.recordProviderFailure(ctx, b, err)
			failures = s.discardSpentSession(ctx, b, failures)
			select {
			case <-time.After(s.backoff()):
			case <-ctx.Done():
				return
			}
			continue
		}
		if msg == nil {
			// An empty long-poll is one full cycle with nothing to do,
			// which is exactly when the discovery credit should move on
			// to the next silent binding.
			failures = 0
			s.alloc.Rotate()
			continue
		}
		if msg.AcquireError == nil {
			failures = 0
		} else {
			// The offers return to the broker's queue, so nothing is
			// lost yet. It counts against the session all the same: a
			// handle whose token refresh has failed answers this call
			// and the poll differently, so a session that keeps failing
			// it while polls succeed would otherwise never be replaced,
			// and the binding would collect offers it can never turn
			// into work forever.
			failures++
			s.log.Error("cannot acquire the jobs this session was offered",
				"binding", b.key, "consecutive_failures", failures, "error", msg.AcquireError)
			// The poll that carried this succeeded and was recorded as
			// contact, so without saying so a binding that can never turn
			// an offer into work reports as reaching its provider.
			s.recordProviderFailure(ctx, b, msg.AcquireError)
			if failures = s.discardSpentSession(ctx, b, failures); b.session == nil {
				// Nothing more goes through a handle that has been given
				// up, and the acknowledgement below is one of those
				// things. The message stays unacknowledged, so the
				// replacement's cursor replays it and its assignments are
				// recorded then.
				continue
			}
		}
		if len(msg.StrandedGrants) > 0 {
			// The broker granted a request this session cannot land: the
			// job is claimed there with nothing here to run it, and no
			// pass on this side can find it again. Nothing to do but say
			// so, loudly enough that it is not discovered as a job that
			// silently never ran.
			s.log.Error("the broker granted jobs this session cannot account for",
				"binding", b.key, "requests", msg.StrandedGrants)
		}

		// Persist before acknowledging. GitHub hands an assignment over
		// once, so a crash after acknowledgement and before the record
		// exists would strand the job with no runner and no local trace.
		// Recording is idempotent, so a redelivered message after a
		// failed acknowledgement changes nothing.
		delivery, err := s.persistDelivery(ctx, b, msg)
		if err != nil {
			s.log.Error("cannot persist the delivery; leaving it for redelivery",
				"binding", b.key, "error", err)
			select {
			case <-time.After(s.backoff()):
			case <-ctx.Done():
				return
			}
			continue
		}
		// Durable before the acknowledgement, for the same reason the
		// delivery is: an acknowledged message is never sent again, and
		// nothing re-derives a cancellation from the provider. One lost
		// here is a whole capsule spent on work the provider already
		// closed. It also has to land before anything schedules, or a
		// cancellation arrives after the attempt it was meant to close
		// has already been leased.
		if err := s.recordLifecycleEvents(ctx, b, msg); err != nil {
			s.log.Error("cannot record the message's lifecycle events; leaving it for redelivery",
				"binding", b.key, "error", err)
			select {
			case <-time.After(s.backoff()):
			case <-ctx.Done():
				return
			}
			continue
		}

		advanced := s.acknowledgeDelivery(ctx, b, assignment.DeliveryID(delivery), msg.ID)

		if msg.Statistics != nil {
			// TotalAssignedJobs is the upstream scaling signal: what
			// GitHub has committed to this scale set. Availables are
			// offers that only become demand once acquired, and counting
			// them would overstate what this binding owes.
			s.alloc.SetAssignedDemand(b.key, msg.Statistics.Assigned)
		}

		s.scheduleReadyAttempts(ctx, b)

		if !advanced {
			// The cursor did not move, so the next poll is answered with
			// this same message immediately. Everything the message
			// carries is processed above either way - a cancellation
			// hint riding in a wedged redelivery still has to land - and
			// the wait is what keeps a binding that cannot advance from
			// becoming an unthrottled call rate against the provider.
			select {
			case <-time.After(s.backoff()):
			case <-ctx.Done():
				return
			}
		}
	}
}

// acknowledgeDelivery drives the durable ack state machine around the
// one network call. The in-flight mark commits before the call, an
// unambiguous success commits confirmed, and anything else commits
// uncertain: the broker's redelivery plus idempotent persistence is
// what converges an uncertain acknowledgement, so no outcome is guessed.
// It reports whether the cursor moved. It does not on two paths - a
// delivery already confirmed, and an acknowledgement that failed - and on
// both of them the next poll returns this same message with no long-poll
// delay, so the caller has to wait rather than spin.
func (s *Controller) acknowledgeDelivery(ctx context.Context, b *binding, deliveryID assignment.DeliveryID, messageID int) bool {
	var proceed bool
	if err := s.store.Tx(ctx, func(tx *store.Tx) error {
		var err error
		proceed, err = tx.AckRequested(deliveryID)
		return err
	}); err != nil {
		s.log.Error("cannot mark the acknowledgement in flight; leaving the message for redelivery",
			"binding", b.key, "error", err)
		return false
	}
	if !proceed {
		// Only a confirmed delivery is excluded now: the broker already
		// knows. A delivery left `requested` by a crash was never
		// acknowledged, so it is retried rather than skipped.
		//
		// Reaching this means the broker sent a delivery this binding had
		// already acknowledged, which it has no reason to do — the
		// acknowledgement is what removes it. Saying so is what turns a
		// binding that has quietly stopped advancing into something with
		// a line explaining it.
		s.log.Warn("the broker redelivered a message this binding already acknowledged; "+
			"the cursor cannot advance past it",
			"binding", b.key, "message", messageID, "delivery", deliveryID)
		return false
	}

	ackErr := b.session.Acknowledge(ctx, messageID)

	// The outcome is recorded on a context detached from the poll: the
	// observation must survive a shutdown that arrives mid-cycle.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if err := s.store.Tx(rctx, func(tx *store.Tx) error {
		if ackErr == nil {
			return tx.AckConfirmed(deliveryID)
		}
		return tx.AckUncertain(deliveryID)
	}); err != nil {
		s.log.Error("cannot record the acknowledgement outcome", "binding", b.key, "error", err)
	}
	if ackErr != nil {
		s.log.Warn("acknowledgement failed; the message will be redelivered",
			"binding", b.key, "error", ackErr)
		return false
	}
	return true
}

// persistDelivery makes one broker message durable: the delivery, its
// attempts and the adapter's observed identifiers commit together, and
// only a committed delivery may be acknowledged. A workload with no
// identity fails the whole message — dropping one and acknowledging the
// rest loses it silently, while an unacknowledged message is
// redelivered, which is visible and recoverable.
//
// A workload that already holds an open attempt is a reassignment
// racing its predecessor. A predecessor that is still ready never
// consumed anything and is superseded in the same transaction; one that
// got further resolves through its own lifecycle first, and this
// message simply stays unacknowledged until it has — never are two
// attempts of one workload live together.
func (s *Controller) persistDelivery(ctx context.Context, b *binding, msg *githubactions.Message) (int64, error) {
	// Everything this message hands over: what the broker assigned plus
	// what the session acquired from its availables.
	admitted := make([]assignment.WorkloadAssignment, 0, len(msg.Assigned)+len(msg.Acquired))
	admitted = append(admitted, msg.Assigned...)
	admitted = append(admitted, msg.Acquired...)
	for _, a := range admitted {
		if err := a.Validate(); err != nil {
			return 0, err
		}
	}
	key := assignment.DeliveryKey(b.scaleSetID, msg.ID)
	// The fingerprint covers the broker's own payload only. It exists to
	// catch the provider sending different content under a stable key, and
	// an acquisition is not the provider changing anything — what it grants
	// depends on who else asked, so including it made the fingerprint vary
	// between redeliveries of one message id. That read as unrecoverable
	// drift, failed the persist, left the message unacknowledged, and
	// because the queue is ordered it blocked the binding permanently.
	fingerprint := assignment.Fingerprint(msg.Assigned)
	workloads := make([]store.WorkloadRow, len(admitted))
	for i, a := range admitted {
		workloads[i] = store.WorkloadRow{
			SourceWorkloadKey: a.SourceWorkloadKey,
			TenantKey:         a.TenantKey,
			ProjectKey:        a.ProjectKey,
		}
	}

	var deliveryID assignment.DeliveryID
	err := s.store.Tx(ctx, func(tx *store.Tx) error {
		id, err := tx.RecordDelivery(b.bindingID, key, fingerprint, workloads)
		if errors.Is(err, store.ErrOpenAttemptExists) {
			// Supersede the predecessor and record again, in this same
			// transaction. Only a predecessor that provably consumed
			// nothing gives way; anything further along refuses, the
			// error propagates, and the message waits as a redelivery.
			//
			// The sweep covers every workload the delivery carries
			// because any of them may hold the conflict, and the ones
			// that do not are no-ops. It cannot include this delivery's
			// own attempts: those were inserted moments ago, above,
			// before the conflicting workload was reached.
			for _, a := range admitted {
				serr := tx.SupersedeOpenAttempt(b.bindingID, assignment.SourceWorkloadKey(a.SourceWorkloadKey),
					assignment.ResolutionSuperseded, id)
				if serr != nil && !errors.Is(serr, store.ErrNotFound) {
					return serr
				}
			}
			id, err = tx.RecordDelivery(b.bindingID, key, fingerprint, workloads)
		}
		if err != nil {
			return err
		}
		deliveryID = id

		// The provider's identifiers ride along per attempt, in the
		// adapter-owned metadata table. Zero request ids are stored as
		// observed — they are diagnostics, never identity.
		attempts, err := tx.AttemptsOfDelivery(id)
		if err != nil {
			return err
		}
		byKey := make(map[string]assignment.WorkloadAssignment, len(admitted))
		for _, a := range admitted {
			byKey[a.SourceWorkloadKey] = a
		}
		for _, attempt := range attempts {
			a, ok := byKey[string(attempt.SourceWorkloadKey)]
			if !ok {
				continue
			}
			if err := tx.RecordGitHubAttemptMetadata(attempt.ID,
				a.SourceWorkloadKey, a.SourceRequestID, a.SourceRunID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return int64(deliveryID), nil
}

// recordLifecycleEvents persists the provider's started/completed
// observations against the attempt they belong to.
// It returns on the first failure. The events after it are not lost:
// the message stays unacknowledged, the broker sends it again, and
// recording is idempotent -- so a redelivery replays the whole message
// rather than the tail of one.
func (s *Controller) recordLifecycleEvents(ctx context.Context, b *binding, msg *githubactions.Message) error {
	// One correlation per event, and everything the event causes acts on
	// what it resolved. Cancelling by workload key instead was a second,
	// independent answer to the same question: after a requeue it named
	// the successor, so a late cancellation of the run that preceded it
	// closed work that had just been handed to this instance.
	record := func(ev assignment.WorkloadLifecycleEvent, kind, idempotency string, cancel bool) error {
		if err := s.store.Tx(ctx, func(tx *store.Tx) error {
			attemptID, err := s.attemptForObservation(tx, b, ev)
			if err != nil || attemptID == "" {
				return err
			}
			if cancel {
				// The cancellation and the event it explains commit
				// together, so no row is closed without the trail that
				// says what closed it.
				if err := s.cancelIfReady(tx, assignment.AttemptID(attemptID)); err != nil {
					return err
				}
			}
			return tx.RecordEvent(assignment.AttemptID(attemptID), idempotency, kind)
		}); err != nil {
			s.log.Error("cannot record a lifecycle event",
				"binding", b.key, "workload", ev.SourceWorkloadKey, "error", err)
			return err
		}
		return nil
	}
	// The hint keys carry the runtime, as cleanup's carry the lease: an
	// attempt is served once per runtime, and a fixed key would swallow a
	// second serving's hints as replays of the first. The cancellation
	// key stays fixed on purpose - it is the same key CancelReady writes,
	// which is what keeps the pair one event, and a cancellation ends the
	// attempt rather than one serving of it.
	for _, ev := range msg.Started {
		s.log.Info("workload started",
			"binding", b.key, "workload", ev.SourceWorkloadKey, "runtime", ev.RuntimeName)
		if err := record(ev, "running_observed", "remote_running_observed:"+string(ev.RuntimeName), false); err != nil {
			return err
		}
	}
	for _, ev := range msg.Completed {
		s.log.Info("workload completed (hint)",
			"binding", b.key, "workload", ev.SourceWorkloadKey, "runtime", ev.RuntimeName, "result", ev.Result)
		// An idempotency key, not a runtime name: the same concatenation
		// one line up converts, and this one did not, so the key carried
		// the type of the thing it names.
		kind, idempotency := "exit_observed", "remote_exit_observed:"+string(ev.RuntimeName)
		if ev.Result == "canceled" {
			kind, idempotency = "remote_canceled", "remote_canceled"
		}
		if err := record(ev, kind, idempotency, ev.Result == "canceled"); err != nil {
			return err
		}
	}
	return nil
}

// cancelIfReady closes the attempt the cancellation was correlated to,
// when it has not begun serving: a provider cancellation of unstarted
// work needs no runtime, no drain and no review. An attempt past ready is
// running work - its cancellation is the drain path's business, so the
// row is left alone and only the event is recorded.
//
// It runs in the caller's transaction, on the attempt the caller
// resolved. Resolving one of its own is how a cancellation of a run that
// has already been superseded reached the successor instead.
func (s *Controller) cancelIfReady(tx *store.Tx, attemptID assignment.AttemptID) error {
	err := tx.CancelReady(attemptID, assignment.ResolutionRemoteCanceled)
	if errors.Is(err, store.ErrConflict) {
		return nil // nothing ready to cancel; running work drains instead
	}
	return err
}

// scheduleReadyAttempts claims ready attempts while admission has capacity.
// The queue is the store, not memory, so a restart resumes
// exactly where this left off, and the claim is a compare-and-swap only
// one caller can win.
func (s *Controller) scheduleReadyAttempts(ctx context.Context, b *binding) {
	b.mu.Lock()
	defer b.mu.Unlock()

	// The disk-pressure admission gate. Ready attempts are durable and
	// lose nothing by waiting; what an emergency must never do is stack
	// more work onto a filesystem that is running out. The monitor logs
	// the closure once, at the transition — not here, every pass.
	if s.currentPressure().AdmissionClosed() {
		return
	}

	var ready []store.Attempt
	if err := s.store.Tx(ctx, func(tx *store.Tx) error {
		var err error
		ready, err = tx.ReadyAttempts(b.bindingID)
		return err
	}); err != nil {
		s.log.Error("cannot read the ready attempts", "binding", b.key, "error", err)
		return
	}

	for _, attempt := range ready {
		if !s.alloc.TryReserve(b.key) {
			return // admission is full; the attempt stays durable and waits
		}
		lease, err := s.createLease(ctx, b, attempt)
		if err != nil {
			s.alloc.Release(b.key)
			if errors.Is(err, store.ErrConflict) {
				continue // another pass claimed it; nothing is lost
			}
			s.log.Error("lease creation failed", "binding", b.key, "error", err)
			continue
		}
		// Claim before the goroutine exists. The lease is already committed
		// and `reserved` is a live state, so between this commit and
		// runCapsule's own claim the periodic reconciler could see it as
		// ownerless and tear down a job that is starting.
		s.claimLease(lease.ID)
		s.wg.Add(1)
		go s.launch(b, lease)
	}
}

// sessionGrace is how long a run of broker session conflicts stays the
// ordinary shape of a restart. Held on the controller for the same
// reason as backoff below.
func (s *Controller) sessionGrace() time.Duration {
	if s.conflictGrace != 0 {
		return s.conflictGrace
	}
	return sessionConflictGrace
}

// backoff is the pause between failed polls. Held on the controller so a
// test can exercise the failure run without waiting out real seconds.
func (s *Controller) backoff() time.Duration {
	if s.pollBackoff > 0 {
		return s.pollBackoff
	}
	return pollBackoff
}

// attemptForObservation resolves which attempt an observation is
// evidence of.
//
// The event names the workload it is about, and that is the answer
// wherever this instance still holds an attempt for it. Correlating by
// the runtime's name instead answers a different question — which attempt
// the runner was provisioned for — and the two are the same thing only
// while nothing has been requeued. A runner is minted against a scale
// set, not a job, so a runner provisioned for one workload can be handed
// another; recorded by runtime name, that execution lands on the attempt
// that never ran, and the attempt's evidence is confidently wrong.
//
// The runtime name stays as the fallback. An observation about a workload
// this instance never recorded, or whose attempt is already settled, is
// still worth keeping against the attempt whose lease owns that runner.
func (s *Controller) attemptForObservation(tx *store.Tx, b *binding,
	ev assignment.WorkloadLifecycleEvent) (assignment.AttemptID, error) {
	attempt, err := tx.OpenAttemptByWorkload(b.bindingID, assignment.SourceWorkloadKey(ev.SourceWorkloadKey))
	switch {
	case err == nil:
		if ev.RuntimeName == "" {
			return attempt.ID, nil
		}
		ranBy, rerr := tx.AttemptOfRuntimeName(ev.RuntimeName)
		switch {
		case errors.Is(rerr, store.ErrNotFound):
			return attempt.ID, nil // a foreign runtime, or one never named
		case rerr != nil:
			return "", rerr
		case ranBy == attempt.ID:
			return attempt.ID, nil
		}

		// The runner ran for some other attempt. Which one separates the
		// two ways that happens.
		provisioned, perr := tx.Get(ranBy)
		switch {
		case errors.Is(perr, store.ErrNotFound):
			// The books no longer hold the attempt this runner ran for,
			// so nothing establishes that the workloads differ - and
			// that is the one case where the comparison cannot be made.
			// Booking it against the open attempt on an unknown is the
			// mis-attribution this whole branch exists to avoid.
			s.log.Warn("an observation names a runtime whose attempt is gone; it is not recorded",
				"binding", b.key, "workload", ev.SourceWorkloadKey, "runtime", ev.RuntimeName)
			return "", nil
		case perr != nil:
			return "", perr
		case string(provisioned.SourceWorkloadKey) == ev.SourceWorkloadKey:
			// Same workload, earlier attempt: a report of the run that
			// attempt made, arriving after a requeue opened this one in
			// its place. The open attempt has started nothing, and
			// writing an execution against it would say otherwise.
			return ranBy, nil
		}

		// Different workloads, so the provider handed this workload to a
		// runner minted for another. The runner is fungible and the
		// workload is not: the event's own workload is the subject.
		s.log.Warn("a runner is executing a workload it was not provisioned for",
			"binding", b.key, "workload", ev.SourceWorkloadKey, "runtime", ev.RuntimeName,
			"attempt", attempt.ID, "runner_of_attempt", ranBy)
		return attempt.ID, nil
	case !errors.Is(err, store.ErrNotFound):
		return "", err
	}

	if ev.RuntimeName == "" {
		return "", nil
	}
	ranBy, err := tx.AttemptOfRuntimeName(ev.RuntimeName)
	if errors.Is(err, store.ErrNotFound) {
		return "", nil // no lease of ours; a foreign runtime
	}
	if err != nil {
		return "", err
	}
	return ranBy, nil
}

// discardSpentSession gives up a session once a run of failures says the
// handle itself is what is broken, and reports the failure count the
// caller carries on with.
//
// The upstream client refreshes an expired session by itself, and when
// that refresh fails it leaves the handle dead: every later call fails
// the same way, for as long as the process runs, while the process itself
// stays healthy. Retrying a dead handle is not recovery, so a run of
// failures costs the session rather than the binding.
//
// The binding is left without a session rather than holding a spent one.
// A handle this reached is unusable whether or not the close succeeded,
// and leaving it in place is what would have the loop poll and announce
// through it, and shutdown close it a second time and report a broker
// still holding a session nobody holds.
func (s *Controller) discardSpentSession(ctx context.Context, b *binding, failures int) int {
	if failures < receiveFailuresBeforeReopen || b.newSession == nil {
		return failures
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if err := b.session.Close(cctx); err != nil {
		// Expected on the path that brought us here: the session the
		// broker holds is the one that stopped answering.
		s.log.Warn("cannot close the failing message session", "binding", b.key, "error", err)
	}
	b.session = nil
	return 0
}

// openSession gives the binding a session and publishes the backlog it
// opened with, which is what Serve does for the first one. Without that
// the allocator keeps the dead session's last demand figure, and a
// binding recorded at zero demand is one the free credit is never shared
// with - so the queue it is holding never drains.
//
// The message cursor resets with the session. That costs a redelivery of
// whatever was not acknowledged, which is free: recording a delivery is
// idempotent on its natural key, and an unacknowledged message was going
// to be redelivered anyway.
func (s *Controller) openSession(ctx context.Context, b *binding) error {
	session, err := b.newSession(ctx)
	if err != nil {
		return err
	}
	b.session = session
	initial := session.Initial()
	if initial == nil {
		// The broker opened the session without statistics, so there is
		// no figure to publish and the allocator keeps the last one it
		// was told. Printing a zero here would be reporting an
		// observation nobody made.
		s.log.Warn("message session opened without statistics; assigned demand is unchanged",
			"binding", b.key)
		return nil
	}
	s.alloc.SetAssignedDemand(b.key, initial.Assigned)
	s.log.Warn("message session opened", "binding", b.key, "assigned_backlog", initial.Assigned)
	return nil
}

// announce publishes the binding's advertised capacity to its session
// and logs the pool arithmetic when the number moved. Every announce
// sets the session's capacity — the broker holds a total, not a delta —
// but only a change is worth a log line, or a quiet pool would fill the
// log with the same number every poll.
func (s *Controller) announce(b *binding) {
	credit := s.alloc.Advertised(b.key)
	b.session.SetCapacity(credit)
	if credit == b.lastAdvertised {
		return
	}
	b.lastAdvertised = credit
	parallelism, rows := s.alloc.PoolReport(assignment.TierID(b.tier.ID))
	total := 0
	for _, r := range rows {
		total += r.Advertised
	}
	s.log.Info("advertised capacity changed", "binding", b.key, "advertised", credit,
		"tier", b.tier.ID, "tier_parallelism", parallelism, "tier_advertised", total,
		"discovery", s.alloc.DiscoveryHolder(assignment.TierID(b.tier.ID)), "instance_capacity", s.alloc.CapacityReport(),
		"credits", rows)
}
