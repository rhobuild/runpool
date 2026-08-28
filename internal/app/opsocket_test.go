package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rhobuild/runpool/internal/assignment"
	"github.com/rhobuild/runpool/internal/store"
)

// shortTempDir is a directory a unix socket fits in.
//
// t.TempDir names its directory after the test, and a socket address is
// a little over a hundred bytes on every platform this runs on -- so a
// descriptive test name is by itself enough to make the bind fail, with
// an error about an invalid argument that says nothing about length.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "rp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// heldAttempt is a store holding one attempt waiting for a person, and
// the controller that serves its state directory.
func heldAttempt(t *testing.T) (*Controller, string, assignment.AttemptID) {
	t.Helper()
	dir := shortTempDir(t)
	st, err := store.Open(dir, store.DefaultRetryBudget)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

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
	return &Controller{log: slog.New(slog.NewTextHandler(io.Discard, nil)), store: st}, dir, id
}

// listening starts the maintenance socket for the life of the test.
func listening(t *testing.T, s *Controller, dir string) {
	t.Helper()
	ln, err := listenForResolutions(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); s.serveResolutions(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("the maintenance socket outlived its context; a shutdown would wait for it")
		}
	})
}

// TestAServingControllerAppliesAResolution: the decision reaches the
// process that holds the lock, which is what lets an operator settle
// held work without stopping every tenant's CI to do it.
func TestAServingControllerAppliesAResolution(t *testing.T) {
	s, dir, id := heldAttempt(t)
	listening(t, s, dir)

	if _, err := ResolveThroughController(dir, ResolveRequest{
		Attempt: string(id), Decision: DecisionRetry,
		Reason: "checked the provider", Actor: "alice",
	}, nil); err != nil {
		t.Fatalf("resolving through the controller: %v", err)
	}

	var got store.Attempt
	if err := s.store.Tx(t.Context(), func(tx *store.Tx) error {
		var err error
		got, err = tx.Get(id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got.State != store.AttemptReady {
		t.Errorf("attempt = %s; want %s", got.State, store.AttemptReady)
	}
	// The audit names the operator, never the controller that carried
	// the decision for them.
	if got.ReviewedBy != "alice" {
		t.Errorf("reviewed by %q; want the operator who decided", got.ReviewedBy)
	}
}

// TestTheControllerRefusesWhatTheStoreRefuses: the socket carries the
// store's answer rather than inventing one, so an attempt already
// resolved is refused the same way on either transport.
func TestTheControllerRefusesWhatTheStoreRefuses(t *testing.T) {
	s, dir, id := heldAttempt(t)
	listening(t, s, dir)

	req := ResolveRequest{Attempt: string(id), Decision: DecisionRetry,
		Reason: "checked the provider", Actor: "alice"}
	if _, err := ResolveThroughController(dir, req, nil); err != nil {
		t.Fatalf("the first resolution: %v", err)
	}
	_, err := ResolveThroughController(dir, req, nil)
	if err == nil {
		t.Fatal("an attempt already resolved was resolved again")
	}
	if !strings.Contains(err.Error(), "manual review") {
		t.Errorf("the refusal reads %q; want the store's own answer", err)
	}
}

// TestAnUnknownOperationIsRefused: a later verb reaching an older
// controller must not be read as the one it does know.
func TestAnUnknownOperationIsRefused(t *testing.T) {
	s, dir, id := heldAttempt(t)
	listening(t, s, dir)

	err := s.applyResolution(t.Context(), ResolveRequest{
		Op: "cancel", Attempt: string(id), Decision: DecisionRetry,
		Reason: "r", Actor: "alice",
	})
	if err == nil || !strings.Contains(err.Error(), "cancel") {
		t.Fatalf("an unknown operation returned %v; want a refusal naming it", err)
	}
}

// TestNoControllerIsToldApartFromARefusal: the caller writes directly
// only when nothing is listening, and that is the one case it may.
func TestNoControllerIsToldApartFromARefusal(t *testing.T) {
	_, dir, id := heldAttempt(t)

	_, err := ResolveThroughController(dir, ResolveRequest{
		Attempt: string(id), Decision: DecisionRetry, Reason: "r", Actor: "alice",
	}, nil)
	if !errors.Is(err, ErrNoController) {
		t.Fatalf("dialling an absent controller returned %v; want %v", err, ErrNoController)
	}
}

// TestAStaleSocketDoesNotBlockTheNextController: a SIGKILL leaves the
// file behind, and a bind that refused it would need the volume cleaned
// by hand before the controller could serve resolutions again.
func TestAStaleSocketDoesNotBlockTheNextController(t *testing.T) {
	dir := shortTempDir(t)
	path := filepath.Join(dir, SocketFile)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ln, err := listenForResolutions(dir)
	if err != nil {
		t.Fatalf("a stale socket file refused the next controller: %v", err)
	}
	defer ln.Close()
	if _, err := net.Dial("unix", path); err != nil {
		t.Errorf("the replacement socket does not answer: %v", err)
	}
}
