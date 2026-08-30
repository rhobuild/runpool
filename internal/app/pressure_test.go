package app

import (
	"testing"

	"github.com/rhobuild/runpool/internal/disk"
	"github.com/rhobuild/runpool/internal/store"
)

// TestAdmissionClosesUnderPressure: in an emergency no new capsule
// starts, and the work is not lost — the attempts stay durable and are
// served the moment pressure releases. High pressure, by contrast,
// garbage collects but keeps admitting.
func TestAdmissionClosesUnderPressure(t *testing.T) {
	h := newHarness(t, 2)

	if err := h.deliver(demand("job-1", "p1", 1)); err != nil {
		t.Fatal(err)
	}

	h.srv.disk.level.Store(int32(disk.SoftEmergency))
	h.serve()
	if got := h.launchedAttempts(); len(got) != 0 {
		t.Fatalf("launched %v under soft emergency; admission must be closed", got)
	}

	h.srv.disk.level.Store(int32(disk.HardEmergency))
	h.serve()
	if got := h.launchedAttempts(); len(got) != 0 {
		t.Fatalf("launched %v under hard emergency; admission must be closed", got)
	}

	h.srv.disk.level.Store(int32(disk.High))
	h.serve()
	if got := h.launchedAttempts(); len(got) != 1 {
		t.Fatalf("launched %v at high pressure; want the queued attempt served", got)
	}
}

// TestPressureResumesFromTheStore: an emergency declared before a crash
// is still in force after the restart — the monitor's persisted verdict
// is the level a fresh process admits under, not an optimistic normal.
func TestPressureResumesFromTheStore(t *testing.T) {
	h := newHarness(t, 1)

	if err := h.store.Tx(t.Context(), func(tx *store.Tx) error {
		return tx.SetPressure(store.PressureVerdict{Level: disk.SoftEmergency.String()})
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.srv.disk.resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := h.srv.currentPressure(); got != disk.SoftEmergency {
		t.Fatalf("resumed level = %s; want soft_emergency", got)
	}

	if err := h.deliver(demand("job-1", "p1", 1)); err != nil {
		t.Fatal(err)
	}
	h.serve()
	if got := h.launchedAttempts(); len(got) != 0 {
		t.Fatalf("launched %v; a restart must not reopen a closed admission", got)
	}
}
