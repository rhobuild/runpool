package app

import (
	"sync"
	"sync/atomic"

	"github.com/rhobuild/runpool/internal/assignment"
)

// leaseOwnership coordinates the controller goroutines that currently drive
// leases. Claiming prevents the periodic reconciler and the launch path from
// owning the same lease; active tracks drain completion; abandoning tells
// failure recovery that a successor now owns unfinished work.
type leaseOwnership struct {
	mu      sync.Mutex
	claimed map[assignment.LeaseID]struct{}
	active  sync.WaitGroup
	abandon atomic.Bool
}

func newLeaseOwnership() *leaseOwnership {
	return &leaseOwnership{claimed: make(map[assignment.LeaseID]struct{})}
}

func (o *leaseOwnership) claim(leaseID assignment.LeaseID) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, held := o.claimed[leaseID]; held {
		return false
	}
	o.claimed[leaseID] = struct{}{}
	return true
}

func (o *leaseOwnership) release(leaseID assignment.LeaseID) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.claimed, leaseID)
}

func (o *leaseOwnership) addActive() { o.active.Add(1) }

func (o *leaseOwnership) activeDone() { o.active.Done() }

func (o *leaseOwnership) wait() <-chan struct{} {
	done := make(chan struct{})
	go func() {
		o.active.Wait()
		close(done)
	}()
	return done
}

func (o *leaseOwnership) abandonUnfinished() { o.abandon.Store(true) }

func (o *leaseOwnership) resumeRecovery() { o.abandon.Store(false) }

func (o *leaseOwnership) isAbandoning() bool { return o.abandon.Load() }
