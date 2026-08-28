package protocol

import "testing"

// TestTheExitCodeNeverMoves: a capsule that has stopped carries no
// version, so this number is read from every build there has ever been.
//
// The control surface lives on a tmpfs that dies with the container, so
// by the time a controller inspects an aborted capsule there is nothing
// left to declare a protocol. The exit code is the whole account, and a
// bump cannot protect a controller from one that moved: an abort read as
// an ordinary exit settles an attempt that never ran as complete, and
// nothing requeues it.
//
// A controller replaced while capsules run adopts them, so the builds on
// either side of this number are not the same build by design. The
// constant is not updated.
func TestTheExitCodeNeverMoves(t *testing.T) {
	if AbortedExitCode != 79 {
		t.Errorf("AbortedExitCode is %d; every capsule ever built exits 79 to say the runner "+
			"never owned the job, and a controller adopting one reads only this",
			AbortedExitCode)
	}
}

// TestEveryStateWordIsSpelledOnce: a word whose meaning moves takes a new
// spelling and a version bump, and this holds the set that exists.
//
// Both bumps this protocol has had moved what an existing word means, and
// each was safe only because the version moved with it. Repurposing a
// word without a bump is the one change no check downstream can catch: a
// controller reading it has no way to tell which meaning it holds.
//
// Adding a word is a bump too — an older controller meets a state it
// does not know, and this fails first so the version is remembered.
func TestEveryStateWordIsSpelledOnce(t *testing.T) {
	spelled := map[State]string{
		StateBooting:  "booting",
		StateWaiting:  "waiting",
		StateStarting: "starting",
		StateReady:    "ready",
		StateRunning:  "running",
	}
	for state, want := range spelled {
		if string(state) != want {
			t.Errorf("a state word is spelled %q and was %q; a meaning that moved needs a new "+
				"word and a Version bump, not the old word carrying it", state, want)
		}
	}
	prefixes := map[string]string{
		ExitedPrefix:  "exited:",
		FailedPrefix:  "failed:",
		AbortedPrefix: "aborted:",
	}
	for got, want := range prefixes {
		if got != want {
			t.Errorf("a terminal prefix is %q and was %q", got, want)
		}
	}
	if Version != "3" {
		t.Logf("Version is %q: the words above moved with it, and this test is where "+
			"the next reader learns that they must", Version)
	}
}
