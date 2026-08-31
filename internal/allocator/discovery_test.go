package allocator

import "testing"

// TestTheCreditMovesOffAHolderThatFoundWork is the regression for a
// starvation the rotation could not recover from.
//
// Rotation happens when a discovery-flagged poll completes, and a poll
// is flagged only when its binding is the pointer and silent at
// BeginPoll. So a holder that gained demand mid-flight completed an
// unflagged poll, and every poll after it was unflagged too -- the
// pointer stayed put until the holder happened to drain and poll empty,
// which nothing bounds. A tier with one busy binding and one quiet one
// is the ordinary topology, and the quiet one announced zero for as
// long as the busy one kept finding work: exactly the blindness the
// credit exists to end, reintroduced by holding it.
func TestTheCreditMovesOffAHolderThatFoundWork(t *testing.T) {
	a := New()
	a.Register("std", "busy", 2)
	a.Register("std", "quiet", 2)
	a.SessionOpened("busy")
	a.SessionOpened("quiet")

	poll := a.BeginPoll("busy")
	a.SetAssignedDemand("busy", 1) // a real job lands mid-flight
	a.CompletePoll(poll, true, false)

	if h := a.DiscoveryHolder("std"); h != "quiet" {
		t.Fatalf("the credit stayed on %q after its holder found work; the quiet "+
			"binding announces zero until the busy one happens to drain and poll "+
			"empty, which nothing bounds", h)
	}
	if got := a.Advertised("quiet"); got == 0 {
		t.Fatal("the quiet binding still advertises zero with the credit in hand")
	}
}

// TestTheCreditMovesOffAHolderThatAdmittedWork: the same starvation
// arrives through admission rather than demand -- TryReserve on the
// holder, or an adoption on restart.
func TestTheCreditMovesOffAHolderThatAdmittedWork(t *testing.T) {
	for name, gain := range map[string]func(*Allocator){
		"reserved": func(a *Allocator) { a.TryReserve("busy") },
		"adopted":  func(a *Allocator) { a.Adopt("busy") },
	} {
		t.Run(name, func(t *testing.T) {
			a := New()
			a.Register("std", "busy", 2)
			a.Register("std", "quiet", 2)
			a.SessionOpened("busy")
			a.SessionOpened("quiet")
			gain(a)
			if h := a.DiscoveryHolder("std"); h != "quiet" {
				t.Fatalf("after %s work the credit stayed on %q", name, h)
			}
		})
	}
}

// TestTheCreditStaysWhenNobodyElseIsSilent: a pointer with nowhere to go
// stays, and the pre-existing recovery -- the holder draining and
// completing an empty flagged poll -- still applies. Moving it to
// another busy binding would be motion without meaning.
func TestTheCreditStaysWhenNobodyElseIsSilent(t *testing.T) {
	a := New()
	a.Register("std", "busy", 2)
	a.Register("std", "also-busy", 2)
	a.SessionOpened("busy")
	a.SessionOpened("also-busy")
	a.SetAssignedDemand("also-busy", 1)
	a.SetAssignedDemand("busy", 1)
	if h := a.DiscoveryHolder("std"); h != "busy" {
		t.Fatalf("the pointer moved to %q with nobody silent to receive it", h)
	}
	// The holder drains while its sibling is still busy: an empty
	// flagged poll has nowhere to rotate to, and staying put is right --
	// the drained holder is silent again and can use the credit itself.
	a.SetAssignedDemand("busy", 0)
	poll := a.BeginPoll("busy")
	a.CompletePoll(poll, true, true)
	if h := a.DiscoveryHolder("std"); h != "busy" {
		t.Fatalf("with nobody else silent the pointer moved to %q", h)
	}
	// The sibling drains too: the next empty flagged poll rotates, which
	// is the pre-existing path, unchanged.
	a.SetAssignedDemand("also-busy", 0)
	poll = a.BeginPoll("busy")
	a.CompletePoll(poll, true, true)
	if h := a.DiscoveryHolder("std"); h != "also-busy" {
		t.Fatalf("an empty flagged poll from the drained holder rotated to %q; "+
			"want the sibling", h)
	}
}

// TestAnInFlightFlaggedPollCannotRotateTwice: the generation bump on an
// advance is what keeps a poll flagged under the old holder from moving
// the pointer a second time when it completes.
func TestAnInFlightFlaggedPollCannotRotateTwice(t *testing.T) {
	a := New()
	a.Register("std", "busy", 2)
	a.Register("std", "quiet", 2)
	a.Register("std", "third", 2)
	a.SessionOpened("busy")
	a.SessionOpened("quiet")
	a.SessionOpened("third")

	poll := a.BeginPoll("busy")    // flagged under generation g
	a.SetAssignedDemand("busy", 1) // advance: pointer -> quiet, generation g+1
	before := a.DiscoveryHolder("std")
	a.CompletePoll(poll, true, true) // stale flagged poll completes empty
	if after := a.DiscoveryHolder("std"); after != before {
		t.Fatalf("a stale flagged poll rotated the pointer %q -> %q past the "+
			"binding the advance chose", before, after)
	}
}

// TestTheCreditMovesUnderAGlobalBudget: the pointer lives on the
// instance-wide ring when a global parallelism is set, and the advance
// has to move that one. Observed through the announcement itself, which
// is what the broker sees: after the holder finds work, the quiet
// binding's poll is the one that carries the discovery capacity.
func TestTheCreditMovesUnderAGlobalBudget(t *testing.T) {
	a := NewWithGlobalParallelism(4)
	a.Register("std", "busy", 2)
	a.Register("heavy", "quiet", 2)
	a.SessionOpened("busy")
	a.SessionOpened("quiet")
	a.SetAssignedDemand("busy", 1)
	if got := a.Advertised("quiet"); got == 0 {
		t.Fatal("after the global holder found work, the quiet binding still " +
			"announces zero; the instance-wide pointer did not move")
	}
}

// TestTheThawDoesNotLeaveTheCreditOnABusyHolder: during a hold the
// advance is rightly a no-op -- everyone is held, there is nobody to
// pass the credit to -- but bindings keep gaining demand while held,
// and disk pressure is what drives holds on a long-running process. A
// pointer parked on a binding that got busy during the hold is the same
// unbounded starvation, arriving through the thaw.
func TestTheThawDoesNotLeaveTheCreditOnABusyHolder(t *testing.T) {
	a := New()
	a.Register("std", "busy", 3)
	a.Register("std", "quiet", 3)
	a.SessionOpened("busy")
	a.SessionOpened("quiet")

	a.Hold(true)
	a.SetAssignedDemand("busy", 1) // work lands while everything is held
	if h := a.DiscoveryHolder("std"); h != "busy" {
		t.Fatalf("during the hold the pointer moved to %q with every candidate held", h)
	}
	a.Hold(false)
	if h := a.DiscoveryHolder("std"); h != "quiet" {
		t.Fatalf("after the thaw the credit sits on %q, whose demand arrived during "+
			"the hold; the quiet binding announces zero until the holder happens to "+
			"drain and poll empty, which nothing bounds", h)
	}
}

// TestTheThawChecksTheGlobalRingToo: the same seam on the instance-wide
// pointer under a global budget.
func TestTheThawChecksTheGlobalRingToo(t *testing.T) {
	a := NewWithGlobalParallelism(4)
	a.Register("std", "busy", 2)
	a.Register("heavy", "quiet", 2)
	a.SessionOpened("busy")
	a.SessionOpened("quiet")

	a.Hold(true)
	a.SetAssignedDemand("busy", 1)
	a.Hold(false)
	if got := a.Advertised("quiet"); got == 0 {
		t.Fatal("after the thaw the quiet binding still announces zero; the " +
			"instance-wide pointer stayed on the binding that got busy during the hold")
	}
}
