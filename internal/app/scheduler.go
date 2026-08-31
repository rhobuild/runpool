package app

import (
	"context"
	"errors"
	"log/slog"

	"github.com/rhobuild/runpool/internal/allocator"
	"github.com/rhobuild/runpool/internal/disk"
	"github.com/rhobuild/runpool/internal/store"
)

// maxReadyAttemptBatch amortizes queue reads without letting one scheduling
// pass retain an arbitrarily large result set or monopolize SQLite.
const maxReadyAttemptBatch = 64

// attemptScheduler owns admission from the durable ready queue. It observes
// capacity and pressure, claims one attempt and lease atomically, and hands a
// committed lease to the executor. It does not poll providers or run capsules.
type attemptScheduler struct {
	log         *slog.Logger
	store       *store.Store
	allocator   *allocator.Allocator
	ownership   *leaseOwnership
	pressure    func() disk.Level
	createLease func(context.Context, *binding, store.Attempt) (store.Lease, error)
	launch      func(*binding, store.Lease)
}

// schedule claims ready attempts while admission has capacity. The queue is
// the store, not memory, so a restart resumes exactly where this left off. A
// binding serializes scheduling passes; the store CAS remains the final
// authority when independent callers race.
func (s *attemptScheduler) schedule(ctx context.Context, binding *binding) {
	binding.mu.Lock()
	defer binding.mu.Unlock()

	if s.pressure().AdmissionClosed() {
		return
	}

	for {
		batchSize := s.allocator.ReservableCapacity(binding.key)
		if batchSize == 0 {
			return
		}
		if batchSize > maxReadyAttemptBatch {
			batchSize = maxReadyAttemptBatch
		}

		var ready []store.Attempt
		if err := s.store.Tx(ctx, func(tx *store.Tx) error {
			var err error
			ready, err = tx.ReadyAttemptBatch(binding.bindingID, batchSize)
			return err
		}); err != nil {
			s.log.Error("cannot read the ready attempts", "binding", binding.key, "error", err)
			return
		}
		if len(ready) == 0 {
			return
		}

		for _, attempt := range ready {
			if !s.admit(ctx, binding, attempt) {
				return
			}
		}
		if len(ready) < batchSize {
			return
		}
	}
}

// admit turns one ready attempt into an executing lease. It reports false only
// when capacity is full; a CAS conflict is ordinary concurrent progress and a
// failed lease creation returns the reserved credit before scheduling
// continues.
func (s *attemptScheduler) admit(ctx context.Context, binding *binding, attempt store.Attempt) bool {
	if !s.allocator.TryReserve(binding.key) {
		return false
	}
	lease, err := s.createLease(ctx, binding, attempt)
	if err != nil {
		s.allocator.Release(binding.key)
		if !errors.Is(err, store.ErrConflict) {
			s.log.Error("lease creation failed", "binding", binding.key, "error", err)
		}
		return true
	}

	// Claim before the goroutine exists. A committed reserved lease is visible
	// to reconciliation, so publishing ownership first prevents startup repair
	// from dismantling work that is beginning locally.
	if !s.ownership.claim(lease.ID) {
		s.log.Error("new lease already has an owner", "binding", binding.key, "lease", lease.ID)
		return true
	}
	s.ownership.addActive()
	go func() {
		defer s.ownership.activeDone()
		s.launch(binding, lease)
	}()
	return true
}
