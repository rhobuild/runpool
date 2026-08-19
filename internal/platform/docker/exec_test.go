package docker

import (
	"context"
	"net"
	"runtime"
	"testing"
	"time"

	"github.com/moby/moby/client"
)

// TestCloseOnDoneClosesTheConnection: cancelling the context has to
// reach the hijacked connection itself.
//
// The client's context governs the dial and the protocol upgrade and
// stops there — once the connection is hijacked, a read on it blocks for
// as long as the container takes, and the deferred close cannot help
// because it is deferred inside the call that is blocked. An exec into a
// wedged container is unbounded without this, and one of those is held
// across the lock every launch waits on.
func TestCloseOnDoneClosesTheConnection(t *testing.T) {
	ours, theirs := net.Pipe()
	defer theirs.Close()
	attach := client.ExecAttachResult{HijackedResponse: client.NewHijackedResponse(ours, "")}

	ctx, cancel := context.WithCancel(t.Context())
	stop := closeOnDone(ctx, attach)
	defer stop()

	read := make(chan error, 1)
	go func() {
		buf := make([]byte, 1)
		_, err := theirs.Read(buf)
		read <- err
	}()

	select {
	case <-read:
		t.Fatal("the read returned before the context was cancelled")
	case <-time.After(50 * time.Millisecond):
	}

	cancel()
	select {
	case <-read:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelling the context did not reach the connection; the read is unbounded")
	}
}

// TestCloseOnDoneStopsWatching: the watcher must not outlive the call
// it belongs to.
//
// It exists only for the duration of one exec, and its context can be a
// long one — the tier's ceiling, or a launch budget of minutes. A
// watcher left running holds a goroutine and a connection reference for
// all of it, on every exec the controller makes.
//
// What it does at the moment it stops is deliberately not asserted: the
// only thing it ever does is close a connection its own call closes on
// the next deferred line, so a close that races the stop costs nothing.
func TestCloseOnDoneStopsWatching(t *testing.T) {
	// A context nothing cancels, so only the stop can end a watcher.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	before := runtime.NumGoroutine()
	for range 64 {
		ours, theirs := net.Pipe()
		attach := client.ExecAttachResult{HijackedResponse: client.NewHijackedResponse(ours, "")}
		closeOnDone(ctx, attach)()
		ours.Close()
		theirs.Close()
	}

	// Goroutines end asynchronously, so this settles rather than reads
	// once; what it must not do is settle above the baseline.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before+8 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("goroutines went from %d to %d after 64 stopped watchers; each exec leaks one "+
		"until its context expires", before, runtime.NumGoroutine())
}
