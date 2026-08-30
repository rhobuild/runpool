package githubactions

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/actions/scaleset"
	"github.com/rhobuild/runpool/internal/assignment"
)

// Session is one message-session lifecycle for one scale set.
//
// Announced capacity is admission — proven live: while the session
// advertises free capacity the broker assigns queued jobs directly, with
// no per-job acceptance step. Offers are the other shape and do take one,
// which Receive answers; capacity is what decides whether either arrives.
// The allocator therefore owns SetCapacity, and this type only reports
// what happens. The upstream client refreshes an expired session by
// itself.
type Session struct {
	mc       *scaleset.MessageSessionClient
	initial  *Statistics
	capacity atomic.Int64
	lastID   assignment.SourceDeliveryID
}

func (c *Client) OpenSession(ctx context.Context, scaleSetID int, owner string) (*Session, error) {
	mc, err := c.c.MessageSessionClient(ctx, scaleSetID, owner)
	if err != nil {
		return nil, fmt.Errorf("open message session: %w", err)
	}
	return &Session{mc: mc, initial: fromStatistics(mc.Session().Statistics)}, nil
}

// IsSessionConflict reports whether err is the broker's 409 refusal
// because a scale set already has an active session. After a controller
// crash the broker holds the dead session until it expires by
// inactivity, so a successor that already owns the local lock must wait
// and retry rather than fail. A live duplicate is impossible: the state
// lock admits one controller.
//
// The status is the durable half of this. The upstream client renders it
// into the error itself, so it is a shape the library owns; the sentence
// beside it is GitHub's own prose, and a crash-recovery path resting on
// that alone would disappear the day a server message is reworded.
// Both are accepted, and the live contract is what keeps saying that at
// least one of them still arrives.
//
// Only the session-open path consults this, so a 409 here is that
// refusal and not some other conflict.
func IsSessionConflict(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, `status="409`) ||
		strings.Contains(msg, "already has an active session")
}

// Initial is the statistics snapshot taken when the session opened; on
// restart it already carries the assigned backlog this instance must
// serve before granting capacity elsewhere.
func (s *Session) Initial() *Statistics { return s.initial }

// SetCapacity changes the free capacity announced on the next poll.
// Zero announces no room: the broker stops assigning new work.
func (s *Session) SetCapacity(n int) { s.capacity.Store(int64(n)) }

// Receive runs one long-poll cycle: announce the current capacity,
// translate the message, and acquire any offered availables. It does
// NOT acknowledge — the caller must make the assignments durable first
// and then call Acknowledge, because the broker hands an assignment over
// once and a crash in between strands the job.
//
// An empty long poll returns (nil, nil).
func (s *Session) Receive(ctx context.Context) (*Message, error) {
	msg, err := s.mc.GetMessage(ctx, int(s.lastID), int(s.capacity.Load()))
	if err != nil {
		return nil, err
	}
	if msg == nil {
		return nil, nil
	}

	out, available := translate(msg)
	// Acquisition is the one place the runner request id is used as an id
	// at all — it is what AcquireJobs takes — and it stays inside this
	// adapter.
	ids, byID := offerable(available)
	if len(ids) > 0 {
		acquired, err := s.mc.AcquireJobs(ctx, ids)
		// A failed acquisition costs the offers in this message and
		// nothing else: they stay queued upstream and are offered again.
		// The assignments alongside them are already this binding's work,
		// and dropping the whole message to report the failure would
		// leave it unacknowledged at the head of an ordered queue — one
		// refusal would stop the binding instead of one acquisition.
		if err != nil {
			out.AcquireError = fmt.Errorf("acquire %d offered jobs: %w", len(ids), err)
			acquired = nil
		}
		mergeAcquired(&out, acquired, byID)
	}

	return &out, nil
}

// offerable decides which availables may be offered for acquisition, and
// pairs each with the request id that will name it in the answer. The
// order is the broker's own, so what is offered reads the same way it
// arrived.
//
// A workload whose id another available in the same message also carries
// is left out. The request id is the only handle AcquireJobs speaks, and
// the domain type is explicit that it is never an identity — duplicates
// are not excluded — so a grant for a shared id names one of them and
// the wire does not say which. Claiming the first would run a job that
// was not granted while losing the one that was. Left out, the workload
// is simply not acquired, which is indistinguishable from an offer the
// broker declined: it stays queued upstream and is offered again.
func offerable(available []assignment.WorkloadAssignment) ([]int64, map[int64]assignment.WorkloadAssignment) {
	carried := make(map[int64]int, len(available))
	for _, j := range available {
		carried[j.SourceRequestID]++
	}
	ids := make([]int64, 0, len(available))
	byID := make(map[int64]assignment.WorkloadAssignment, len(available))
	for _, j := range available {
		if carried[j.SourceRequestID] != 1 {
			continue
		}
		ids = append(ids, j.SourceRequestID)
		byID[j.SourceRequestID] = j
	}
	return ids, byID
}

// Acknowledge tells the broker the message is safely handled and advances
// the cursor. Until it is called the same message is redelivered, which
// is what makes the persist-then-acknowledge order crash-safe.
func (s *Session) Acknowledge(ctx context.Context, messageID assignment.SourceDeliveryID) error {
	if err := s.mc.DeleteMessage(ctx, int(messageID)); err != nil {
		return fmt.Errorf("acknowledge message %d: %w", messageID, err)
	}
	s.lastID = messageID
	return nil
}

func (s *Session) Close(ctx context.Context) error { return s.mc.Close(ctx) }

// mergeAcquired folds the broker's grants into the message. One grant,
// one workload: the pairing is spent when it is used, so a second grant
// for the same id is stranded rather than claiming the workload twice —
// a broker that answers with a duplicate id would otherwise have one CI
// job admitted twice, and the strand is also what makes the duplicate
// visible instead of silently absorbed.
func mergeAcquired(out *Message, acquired []int64, byID map[int64]assignment.WorkloadAssignment) {
	for _, id := range acquired {
		j, ok := byID[id]
		if !ok {
			out.StrandedGrants = append(out.StrandedGrants, id)
			continue
		}
		delete(byID, id)
		out.Acquired = append(out.Acquired, j)
	}
}
