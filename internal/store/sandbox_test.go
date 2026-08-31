package store

import (
	"testing"
	"time"
)

// TestNoSandboxPassIsNotAHealthyOne: absent and healthy are different
// answers, and the difference matters more here than in most places. An
// instance that has never completed a pass is one whose gateways are in
// whatever state its startup left them, and a reader told "no error" is
// told the pass ran and found nothing wrong.
func TestNoSandboxPassIsNotAHealthyOne(t *testing.T) {
	s := newStore(t)
	var got *SandboxPass
	inTx(t, s, func(tx *Tx) error {
		var err error
		got, err = tx.SandboxPass()
		return err
	})
	if got != nil {
		t.Errorf("a store with no pass reported %+v; want nil, which a reader has to "+
			"treat as unknown rather than as a pass that found nothing", got)
	}
}

// TestTheLastSandboxPassIsWhatIsRead: the record is a singleton, so a
// pass replaces the one before it. A reader that saw the first failure
// forever would send an operator after a gateway that reopened an hour
// ago.
func TestTheLastSandboxPassIsWhatIsRead(t *testing.T) {
	s := newStore(t)
	write := func(p SandboxPass) {
		t.Helper()
		inTx(t, s, func(tx *Tx) error { return tx.SetSandboxPass(p) })
	}
	read := func() *SandboxPass {
		t.Helper()
		var got *SandboxPass
		inTx(t, s, func(tx *Tx) error {
			var err error
			got, err = tx.SandboxPass()
			return err
		})
		return got
	}

	write(SandboxPass{At: time.Unix(1000, 0), Error: "the probe could not run"})
	if got := read(); got == nil || !got.At.Equal(time.Unix(1000, 0)) || got.Error != "the probe could not run" {
		t.Fatalf("read back %+v; want the failure that was written", got)
	}
	// Recovery has to be readable, not only failure. A pass that
	// succeeds must clear the reason, or a gateway that reopened still
	// reports as closed.
	write(SandboxPass{At: time.Unix(2000, 0)})
	got := read()
	if got == nil || !got.At.Equal(time.Unix(2000, 0)) {
		t.Fatalf("read back %+v; want the later pass", got)
	}
	if got.Error != "" {
		t.Errorf("a successful pass left %q behind; a sandbox that recovered would "+
			"keep reporting the failure it recovered from", got.Error)
	}
}
