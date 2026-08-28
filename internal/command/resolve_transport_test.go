package command

import (
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rhobuild/runpool/internal/app"
	"github.com/rhobuild/runpool/internal/assignment"
	"github.com/rhobuild/runpool/internal/store"
)

// heldIn seeds a state directory with one attempt waiting for a person.
//
// The directory is short on purpose: a unix socket address is a little
// over a hundred bytes, and t.TempDir names its directory after the
// test, which is enough on its own to make a bind fail.
func heldIn(t *testing.T) (string, assignment.AttemptID) {
	t.Helper()
	dir, err := os.MkdirTemp("", "rp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	st, err := store.Open(dir, store.DefaultRetryBudget)
	if err != nil {
		t.Fatal(err)
	}
	var id assignment.AttemptID
	if err := st.Tx(t.Context(), func(tx *store.Tx) error {
		binding, err := tx.EnsureBinding("app", "github_actions",
			"v1|repository|https://github.com/acme/app||runpool-standard")
		if err != nil {
			return err
		}
		if _, err := tx.RecordDelivery(binding, "msg-1", [32]byte{},
			[]store.WorkloadRow{{SourceWorkloadKey: "job-1", TenantKey: "acme", ProjectKey: "app"}}); err != nil {
			return err
		}
		ready, err := tx.ReadyAttempts(binding)
		if err != nil {
			return err
		}
		id = ready[0].ID
		return tx.HoldForReview(id, store.ReviewReasonStartOutcomeUnknown)
	}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	t.Setenv("RUNPOOL_STATE_DIR", dir)
	return dir, id
}

// discardIO is the command's streams thrown away: these tests read the
// store, not the transcript.
func discardIO() IO { return IO{In: strings.NewReader(""), Out: io.Discard, Err: io.Discard} }

func decodeJSON(r io.Reader, v any) error { return json.NewDecoder(r).Decode(v) }
func encodeJSON(w io.Writer, v any) error { return json.NewEncoder(w).Encode(v) }

func stateOf(t *testing.T, dir string, id assignment.AttemptID) store.AttemptState {
	t.Helper()
	st, err := store.OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var got store.Attempt
	if err := st.Tx(t.Context(), func(tx *store.Tx) error {
		var err error
		got, err = tx.Get(id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return got.State
}

// TestAStoppedControllerIsResolvedDirectly: the offline path is the one
// v1.0.0 had, and it is what a resolution falls back to when nothing is
// serving. It must not be simplified away with the socket in place —
// there is no controller to carry the decision when the controller is
// what the operator stopped.
func TestAStoppedControllerIsResolvedDirectly(t *testing.T) {
	dir, id := heldIn(t)

	if err := runAttemptsResolve(discardIO(), string(id), true, false,
		"checked the provider", "alice", true); err != nil {
		t.Fatalf("resolving with no controller running: %v", err)
	}
	if got := stateOf(t, dir, id); got != store.AttemptReady {
		t.Errorf("attempt = %s; want %s", got, store.AttemptReady)
	}
}

// TestALiveControllerThatCannotCarryOneSaysSo: the lock is held and
// nothing answers the socket. Writing anyway would be a second writer
// against a store whose whole concurrency model is one, so the refusal
// has to name what to do instead.
func TestALiveControllerThatCannotCarryOneSaysSo(t *testing.T) {
	dir, id := heldIn(t)
	lock, err := store.TryAcquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()

	err = runAttemptsResolve(discardIO(), string(id), true, false,
		"checked the provider", "alice", true)
	if err == nil {
		t.Fatal("a resolution wrote beside a process holding the lock")
	}
	if !strings.Contains(err.Error(), "does not answer resolutions") {
		t.Errorf("the refusal reads %q; an operator needs to know the controller is the obstacle", err)
	}
	if got := stateOf(t, dir, id); got != store.AttemptManualReview {
		t.Errorf("attempt = %s; the refused resolution wrote anyway", got)
	}
}

// TestAServingControllerCarriesTheResolution: with something listening,
// the decision travels there and the operator's own store stays closed.
func TestAServingControllerCarriesTheResolution(t *testing.T) {
	dir, id := heldIn(t)
	lock, err := store.TryAcquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()

	ln, err := net.Listen("unix", filepath.Join(dir, app.SocketFile))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	carried := make(chan app.ResolveRequest, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var req app.ResolveRequest
		if err := decodeJSON(conn, &req); err != nil {
			return
		}
		carried <- req
		encodeJSON(conn, app.ResolveReply{OK: true})
	}()

	if err := runAttemptsResolve(discardIO(), string(id), true, false,
		"checked the provider", "alice", true); err != nil {
		t.Fatalf("resolving through a serving controller: %v", err)
	}
	got := <-carried
	if got.Attempt != string(id) || got.Decision != app.DecisionRetry ||
		got.Actor != "alice" || got.Reason != "checked the provider" {
		t.Errorf("the controller was sent %+v; the operator's decision did not travel whole", got)
	}
	// The command must not have written: the controller it handed the
	// decision to is the writer.
	if state := stateOf(t, dir, id); state != store.AttemptManualReview {
		t.Errorf("attempt = %s; the command wrote as well as sending", state)
	}
}
