package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/rhobuild/runpool/internal/assignment"
	"github.com/rhobuild/runpool/internal/store"
)

// SocketFile is the controller's maintenance socket, beside the database
// and the lock it already keeps in the state directory.
//
// A held attempt is settled by a person, and the write that settles it
// belongs to whoever holds the lock — which is the controller, whenever
// one is running. So the decision travels to the writer instead of the
// operator taking the lock away from it, and the only place both of them
// already share is the state directory.
//
// It is not an endpoint. There is no port, no route and no name to
// resolve: reaching this file means holding the state directory, which
// is the same reach that already opens the database and takes the lock.
// In the reference deployment that directory is a volume mounted only
// inside the controller's container, and every operator command already
// runs there.
const SocketFile = "runpool.sock"

// ResolveRequest is one operator decision about one held attempt.
//
// Op is carried so an unknown one is refused rather than read as this
// one: a later verb arriving at an older controller must not be
// mistaken for a resolution.
type ResolveRequest struct {
	Op       string `json:"op"`
	Attempt  string `json:"attempt_id"`
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
	Actor    string `json:"actor"`
}

// ResolveReply is what the controller did, or why it did nothing.
type ResolveReply struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// Decisions a resolution may carry. They are the two the command already
// offers, spelled the same way, so an operator reading a transcript and
// an operator reading the help see one vocabulary.
const (
	DecisionRetry  = "retry"
	DecisionSettle = "settle-may-have-run"
	opResolve      = "resolve"
)

// listenForResolutions opens the maintenance socket.
//
// A stale file is removed first: the lock is already held by this
// process, so no other controller is listening, and a socket left by a
// SIGKILL would otherwise refuse the bind for the life of the volume.
//
// It can fail on a path a database opens without complaint. A unix
// socket address is a little over a hundred bytes on every platform
// runpool runs on, so a state directory nested deeply enough is refused
// here and nowhere else -- the reference deployment mounts a volume at
// /var/lib/runpool/state and is nowhere near it, but a bind mount under
// a long home directory can be. The caller warns and serves without it
// rather than refusing to start, because the offline path still exists
// and losing a listener is not worth every tenant's CI.
func listenForResolutions(dir string) (net.Listener, error) {
	path := filepath.Join(dir, SocketFile)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	// The state directory is already the boundary; this narrows the file
	// to the account that runs the controller rather than relying on it.
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// serveResolutions answers operator decisions until the context ends.
//
// One request per connection, one reply, closed: a resolution is rare,
// human-paced and independent, so a session would be state to keep for
// no one.
func (s *Controller) serveResolutions(ctx context.Context, ln net.Listener) {
	go func() {
		<-ctx.Done()
		// Unblocks Accept: the loop below sees the closed listener and
		// returns rather than outliving the shutdown budget.
		ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() == nil {
				s.log.Warn("maintenance socket stopped accepting", "error", err)
			}
			return
		}
		s.answerResolution(ctx, conn)
	}
}

func (s *Controller) answerResolution(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	// A caller that connects and says nothing must not hold the only
	// listener: the operator's own command is subsecond.
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))

	var req ResolveRequest
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		writeReply(conn, ResolveReply{Error: "could not read the request: " + err.Error()})
		return
	}
	err := s.applyResolution(ctx, req)
	if err != nil {
		s.log.Warn("operator resolution refused", "attempt", req.Attempt,
			"decision", req.Decision, "actor", req.Actor, "error", err)
		writeReply(conn, ResolveReply{Error: err.Error()})
		return
	}
	s.log.Info("operator resolution applied", "attempt", req.Attempt,
		"decision", req.Decision, "actor", req.Actor, "reason", req.Reason)
	writeReply(conn, ResolveReply{OK: true})
}

// applyResolution runs the same two store calls the offline command
// runs. Sharing them is what keeps one audit record: the row's reviewer
// and the event's actor are the operator either way, never the
// controller that carried the decision.
func (s *Controller) applyResolution(ctx context.Context, req ResolveRequest) error {
	if req.Op != opResolve {
		return fmt.Errorf("unknown operation %q", req.Op)
	}
	if req.Attempt == "" || req.Reason == "" || req.Actor == "" {
		return errors.New("a resolution names its attempt, its reason and its actor")
	}
	id := assignment.AttemptID(req.Attempt)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return s.store.Tx(ctx, func(tx *store.Tx) error {
		switch req.Decision {
		case DecisionRetry:
			return tx.ResolveReviewToReady(id, req.Reason, req.Actor)
		case DecisionSettle:
			return tx.ResolveReviewToSettled(id, assignment.ResolutionMayHaveExecuted, req.Reason, req.Actor)
		}
		return fmt.Errorf("unknown decision %q", req.Decision)
	})
}

func writeReply(conn net.Conn, reply ResolveReply) {
	_ = json.NewEncoder(conn).Encode(reply)
}

// ResolveThroughController hands one decision to the controller serving
// this state directory, and reports whether it applied it.
//
// ErrNoController distinguishes the two reasons nothing answered: there
// is no controller, so the caller may write directly under the lock, or
// there is one too old to carry a resolution, which the caller finds by
// discovering the lock held.
func ResolveThroughController(dir string, req ResolveRequest, log *slog.Logger) (ResolveReply, error) {
	conn, err := net.DialTimeout("unix", filepath.Join(dir, SocketFile), 5*time.Second)
	if err != nil {
		return ResolveReply{}, fmt.Errorf("%w: %v", ErrNoController, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(60 * time.Second))

	req.Op = opResolve
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return ResolveReply{}, fmt.Errorf("sending the resolution: %w", err)
	}
	var reply ResolveReply
	if err := json.NewDecoder(conn).Decode(&reply); err != nil {
		// The decision may have applied: it travelled, and only the
		// answer was lost. Saying so is the whole value of the message,
		// because the alternative an operator reaches for is to run it
		// again.
		return ResolveReply{}, fmt.Errorf("the resolution was sent but its outcome is unknown (%w); "+
			"read `runpool attempts inspect` before deciding again", err)
	}
	if !reply.OK {
		return reply, errors.New(reply.Error)
	}
	return reply, nil
}

// ErrNoController is returned when nothing is listening on the
// maintenance socket.
var ErrNoController = errors.New("no controller is serving this state directory")
