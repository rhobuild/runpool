package app

import (
	"testing"
	"time"
)

func TestLeaseOwnershipCoordinatesClaimsDrainAndAbandonment(t *testing.T) {
	ownership := newLeaseOwnership()
	if !ownership.claim("lease-a") {
		t.Fatal("first claim was refused")
	}
	if ownership.claim("lease-a") {
		t.Fatal("duplicate claim was accepted")
	}
	ownership.release("lease-a")
	if !ownership.claim("lease-a") {
		t.Fatal("released claim could not be acquired again")
	}

	ownership.addActive()
	drained := ownership.wait()
	select {
	case <-drained:
		t.Fatal("drain completed while one lease goroutine was active")
	default:
	}
	ownership.activeDone()
	select {
	case <-drained:
	case <-time.After(time.Second):
		t.Fatal("drain did not complete after the final lease goroutine finished")
	}

	ownership.abandonUnfinished()
	if !ownership.isAbandoning() {
		t.Fatal("abandonment was not visible to recovery")
	}
	ownership.resumeRecovery()
	if ownership.isAbandoning() {
		t.Fatal("recovery remained abandoned after reset")
	}
}
