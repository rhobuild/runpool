package githubcontract

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/actions/scaleset"
)

// The identity contract behind the delivery/attempt schema.
//
// The delivery is identified by (binding, message id), the attempt by
// (delivery, job id), and runner_request_id is recorded as a diagnostic
// that may legitimately be zero. This suite establishes what that
// composition rests on:
//
//   - a message id is positive and stable across redelivery;
//   - an unacknowledged message is redelivered with identical content,
//     compared by a fingerprint over every field the product processes;
//   - an assignment no runner acquires is cancelled and requeued, and
//     what identity the requeue carries.
//
// Every message is written down, not only the ones a test is waiting
// for. A harness that records assignments and discards the rest cannot
// say afterwards what accompanied them, and the cancellation upstream
// documents alongside a requeue is one of the kinds it would discard.
//
// Discipline the harness holds itself to: one explicit token, never the
// ambient authentication of some CLI; scale sets created under random
// names, adopted never, deleted by the id this run created; the fixture
// workflow verified byte-identical to the versioned copy before any
// dispatch; the one run this suite started is the one run it cancels;
// and everything observed lands in a redacted evidence artifact whether
// the run passes or fails.
//
//	RUNPOOL_CONTRACT_REPO_URL    fixture repository URL
//	RUNPOOL_CONTRACT_REPO_TOKEN  token: self-hosted runner admin + Actions read/write
//	RUNPOOL_CONTRACT_REPO_A      owner/name of the fixture repository
//	RUNPOOL_CONTRACT_ARTIFACT_DIR  where evidence lands; required for qualification
//
// Under RUNPOOL_CONTRACT_QUALIFY a missing credential, fixture or artifact
// destination is a failure rather than a skip.
const (
	identityWorkflow    = "identity.yml"
	identityFixturePath = ".github/workflows/identity.yml"

	kindAvailable = "JobAvailable"
	kindAssigned  = "JobAssigned"
	kindStarted   = "JobStarted"
	kindCompleted = "JobCompleted"
)

// observedJob is one job message of any kind. The schema decision is
// made from recorded values, never from recalled ones.
//
// Result and RunnerName are populated on the lifecycle kinds only; a job
// that no runner ever acquired reports an empty RunnerName, which is
// itself the observation. QueueTime and ScaleSetAssignTime are what
// distinguish a delay the provider imposed from one the fixture's own
// job timeout produced.
type observedJob struct {
	Kind               string    `json:"kind"`
	MessageID          int       `json:"message_id"`
	Owner              string    `json:"owner"`
	Repository         string    `json:"repository"`
	JobID              string    `json:"job_id"`
	RequestID          int64     `json:"runner_request_id"`
	RunID              int64     `json:"workflow_run_id"`
	Result             string    `json:"result,omitempty"`
	RunnerName         string    `json:"runner_name,omitempty"`
	QueueTime          time.Time `json:"queue_time"`
	ScaleSetAssignTime time.Time `json:"scale_set_assign_time"`
	Fingerprint        string    `json:"fingerprint"`
}

// fingerprintOf digests every normalized field the product processes
// from an assignment — not a subset. A redelivery must repeat the
// delivery byte-for-byte in these terms; differing content under the
// same natural key is contract drift, and the product's rule for drift
// is to stop the binding rather than overwrite.
//
// The fields added for observation are deliberately outside it: the
// fingerprint is the redelivery contract, and widening it would change
// what counts as drift.
func (a observedJob) fingerprintOf() string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s|%s|%s|%d|%d",
		a.Owner, a.Repository, a.JobID, a.RequestID, a.RunID))
	return hex.EncodeToString(sum[:])
}

// observedMessage is one broker delivery: its identity, when it was
// seen, the demand the broker reported with it, and every job it
// carried. The statistic rides along because upstream calls it the
// current state of assigned work, so it is the one figure that says
// whether a requeue is the same demand or new demand.
type observedMessage struct {
	MessageID    int           `json:"message_id"`
	ObservedAt   time.Time     `json:"observed_at"`
	AssignedJobs int           `json:"statistics_total_assigned_jobs"`
	Jobs         []observedJob `json:"jobs"`
}

// assigned returns the JobAssigned entries of one message.
func (m observedMessage) assigned() []observedJob {
	var out []observedJob
	for _, j := range m.Jobs {
		if j.Kind == kindAssigned {
			out = append(out, j)
		}
	}
	return out
}

type identityFixture struct {
	client  *scaleset.Client
	rest    *restClient
	session *scaleset.MessageSessionClient
	label   string
	repo    string
	cursor  int

	evidence identityEvidence
}

// identityEvidence is the artifact: everything the run observed, tied to
// the inputs that produced it, written on success and on failure alike.
// It carries ids, digests and timestamps — never tokens.
type identityEvidence struct {
	StartedAt     time.Time         `json:"started_at"`
	Repo          string            `json:"repo"`
	ScaleSetLabel string            `json:"scale_set_label"`
	ScaleSetID    int               `json:"scale_set_id"`
	CorrelationID string            `json:"correlation_id"`
	FixtureSHA256 string            `json:"fixture_sha256"`
	WorkflowRunID int64             `json:"workflow_run_id"`
	Messages      []observedMessage `json:"messages"`
	Cleanup       map[string]string `json:"cleanup"`
	Outcome       string            `json:"outcome"`
}

func qualifying() bool { return os.Getenv("RUNPOOL_CONTRACT_QUALIFY") != "" }

func requireOrSkip(t *testing.T, name, value string) string {
	t.Helper()
	if value == "" {
		if qualifying() {
			t.Fatalf("%s is unset during release qualification; live contracts cannot be skipped", name)
		}
		t.Skipf("%s not set; the identity suite is opt-in", name)
	}
	return value
}

func newIdentityFixture(t *testing.T, ctx context.Context) *identityFixture {
	t.Helper()
	url, token := target(t, envRepoURL, envRepoToken)
	repo := requireOrSkip(t, envRepoA, os.Getenv(envRepoA))
	c := newClient(t, url, token)
	rest := newRESTClient(token)

	f := &identityFixture{client: c, rest: rest, repo: repo}
	f.evidence = identityEvidence{
		StartedAt: time.Now().UTC(),
		Repo:      repo,
		Cleanup:   map[string]string{},
		Outcome:   "incomplete",
	}
	t.Cleanup(func() { f.writeEvidence(t) })

	// The installed fixture must be the reviewed fixture. Dispatching an
	// edited workflow measures whatever that edit does, under this
	// suite's name.
	versioned, err := os.ReadFile(filepath.Join("testdata", identityWorkflow))
	if err != nil {
		t.Fatalf("read the versioned fixture: %v", err)
	}
	wantSum := sha256.Sum256(versioned)
	got, err := rest.installedFixtureDigest(ctx, repo, identityFixturePath)
	if err != nil {
		t.Fatalf("read the installed fixture: %v", err)
	}
	if got != hex.EncodeToString(wantSum[:]) {
		t.Fatalf("the installed %s (sha256 %s) differs from testdata/%s (%s); "+
			"install the versioned fixture before measuring the contract",
			identityFixturePath, got, identityWorkflow, hex.EncodeToString(wantSum[:]))
	}
	f.evidence.FixtureSHA256 = got

	group, err := c.GetRunnerGroupByName(ctx, scaleset.DefaultRunnerGroup)
	if err != nil {
		t.Fatalf("resolve Default runner group: %v", err)
	}

	// A random name per run, created not adopted. If the name is somehow
	// taken, that is a foreign resource and the run fails rather than
	// borrowing it.
	f.label = "runpool-identity-" + randomHex(t, 5)
	f.evidence.ScaleSetLabel = f.label
	existing, err := c.GetRunnerScaleSet(ctx, group.ID, f.label)
	if err != nil {
		t.Fatalf("check the scale set name is free: %v", err)
	}
	if existing != nil {
		t.Fatalf("scale set %q already exists; this run will not adopt a resource it did not create", f.label)
	}
	set, err := c.CreateRunnerScaleSet(ctx, &scaleset.RunnerScaleSet{
		Name: f.label, RunnerGroupID: group.ID,
	})
	if err != nil {
		t.Fatalf("create scale set: %v", err)
	}
	f.evidence.ScaleSetID = set.ID
	createdID := set.ID
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()
		// By id, never by name: the id is the only thing this run knows
		// it created.
		if err := c.DeleteRunnerScaleSet(cctx, createdID); err != nil {
			f.evidence.Cleanup["scale_set"] = err.Error()
			t.Errorf("cleanup: scale set %d was not deleted: %v", createdID, err)
		} else {
			f.evidence.Cleanup["scale_set"] = "deleted"
		}
	})

	session, err := c.MessageSessionClient(ctx, set.ID, "runpool-identity")
	if err != nil {
		t.Fatalf("open message session: %v", err)
	}
	f.session = session
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer ccancel()
		if err := session.Close(cctx); err != nil {
			f.evidence.Cleanup["session"] = err.Error()
			t.Errorf("cleanup: message session was not closed: %v", err)
		} else {
			f.evidence.Cleanup["session"] = "closed"
		}
	})
	return f
}

func randomHex(t *testing.T, n int) string {
	t.Helper()
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("generate a run-scoped id: %v", err)
	}
	return hex.EncodeToString(buf)
}

// dispatch starts the fixture workflow against this run's own label and
// captures the id of the run it created — the only run its cleanup may
// cancel.
func (f *identityFixture) dispatch(t *testing.T, ctx context.Context) {
	t.Helper()
	correlation := randomHex(t, 6)
	f.evidence.CorrelationID = correlation
	if err := f.rest.dispatchWorkflow(ctx, f.repo, identityWorkflow, map[string]string{
		"runner_label":   f.label,
		"correlation_id": correlation,
	}); err != nil {
		t.Fatalf("dispatch %s on %s for label %s: %v", identityWorkflow, f.repo, f.label, err)
	}
	run, err := f.rest.findRunByCorrelation(ctx, f.repo, identityWorkflow, correlation, time.Minute)
	if err != nil {
		t.Fatalf("locate the dispatched run: %v", err)
	}
	f.evidence.WorkflowRunID = run.ID
	t.Logf("dispatched %s on %s targeting %s: run %d", identityWorkflow, f.repo, f.label, run.ID)
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()
		if err := f.rest.cancelRun(cctx, f.repo, run.ID); err != nil {
			f.evidence.Cleanup["workflow_run"] = err.Error()
			t.Errorf("cleanup: run %d was not cancelled: %v", run.ID, err)
		} else {
			f.evidence.Cleanup["workflow_run"] = fmt.Sprintf("cancelled run %d", run.ID)
		}
	})
}

// record turns one broker message into evidence, whatever it carries.
// Recording happens before any decision about the message, so a kind no
// test is waiting for is still written down rather than deleted unseen.
func (f *identityFixture) record(t *testing.T, msg *scaleset.RunnerScaleSetMessage) observedMessage {
	t.Helper()
	out := observedMessage{MessageID: msg.MessageID, ObservedAt: time.Now().UTC()}
	if msg.Statistics != nil {
		out.AssignedJobs = msg.Statistics.TotalAssignedJobs
	}
	add := func(kind string, b scaleset.JobMessageBase, result, runner string) {
		j := observedJob{
			Kind: kind, MessageID: msg.MessageID,
			Owner: b.OwnerName, Repository: b.RepositoryName,
			JobID: b.JobID, RequestID: b.RunnerRequestID, RunID: b.WorkflowRunID,
			Result: result, RunnerName: runner,
			QueueTime: b.QueueTime, ScaleSetAssignTime: b.ScaleSetAssignTime,
		}
		j.Fingerprint = j.fingerprintOf()
		t.Logf("%s: message=%d job=%s request=%d run=%d result=%q runner=%q",
			kind, msg.MessageID, j.JobID, j.RequestID, j.RunID, result, runner)
		out.Jobs = append(out.Jobs, j)
	}
	for _, j := range msg.JobAvailableMessages {
		add(kindAvailable, j.JobMessageBase, "", "")
	}
	for _, j := range msg.JobAssignedMessages {
		add(kindAssigned, j.JobMessageBase, "", "")
	}
	for _, j := range msg.JobStartedMessages {
		add(kindStarted, j.JobMessageBase, "", j.RunnerName)
	}
	for _, j := range msg.JobCompletedMessages {
		add(kindCompleted, j.JobMessageBase, j.Result, j.RunnerName)
	}
	f.evidence.Messages = append(f.evidence.Messages, out)
	return out
}

// await polls with capacity announced for at most within, recording
// every message, and stops when stop reports the caller has seen enough.
// A message stop rejects is acknowledged so the poll makes progress; the
// one that satisfies it is left pending, because leaving a message
// unacknowledged is what the redelivery check rests on.
//
// It reports whether stop was satisfied rather than failing on its own.
// An absence is an observation here — a contract that cannot record "the
// message never came" can only ever measure the cases that went well.
func (f *identityFixture) await(t *testing.T, ctx context.Context, within time.Duration, stop func(observedMessage) bool) bool {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		msg, err := f.session.GetMessage(ctx, f.cursor, 1)
		if err != nil {
			t.Fatalf("get message: %v", err)
		}
		if msg == nil {
			continue // empty long poll
		}
		if stop(f.record(t, msg)) {
			return true
		}
		// Not what this caller waits for: advance past it so the poll
		// makes progress. It is already in the evidence.
		f.cursor = msg.MessageID
		if err := f.session.DeleteMessage(ctx, msg.MessageID); err != nil {
			t.Fatalf("delete an unrelated message %d: %v", msg.MessageID, err)
		}
	}
	return false
}

// awaitAssignments waits for the first message carrying assignments and
// leaves it unacknowledged.
func (f *identityFixture) awaitAssignments(t *testing.T, ctx context.Context, within time.Duration) []observedJob {
	t.Helper()
	var seen []observedJob
	if !f.await(t, ctx, within, func(m observedMessage) bool {
		seen = m.assigned()
		return len(seen) > 0
	}) {
		t.Fatalf("no assignment arrived within %s; the identity contract is unmeasured", within)
	}
	return seen
}

// writeEvidence lands the artifact. During release qualification the
// destination is mandatory and a write failure fails the run: evidence
// that was not archived is evidence that does not exist.
func (f *identityFixture) writeEvidence(t *testing.T) {
	t.Helper()
	if f.evidence.Outcome == "incomplete" && !t.Failed() {
		f.evidence.Outcome = "passed"
	}
	if t.Failed() {
		f.evidence.Outcome = "failed"
	}
	dir := os.Getenv("RUNPOOL_CONTRACT_ARTIFACT_DIR")
	if dir == "" {
		if qualifying() {
			t.Error("RUNPOOL_CONTRACT_ARTIFACT_DIR is unset during release qualification")
		}
		return
	}
	payload, err := json.MarshalIndent(f.evidence, "", "  ")
	if err != nil {
		t.Errorf("encode evidence: %v", err)
		return
	}
	name := fmt.Sprintf("identity-%s-%s.json", f.evidence.StartedAt.Format("20060102T150405Z"), f.evidence.CorrelationID)
	if err := os.WriteFile(filepath.Join(dir, name), payload, 0o644); err != nil {
		t.Errorf("write evidence artifact: %v", err)
	}
}

// TestDeliveryIdentityIsStable establishes what the delivery key rests
// on: a positive message id, and a redelivery that repeats it with an
// identical fingerprint.
func TestDeliveryIdentityIsStable(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	f := newIdentityFixture(t, ctx)
	f.dispatch(t, ctx)

	first := f.awaitAssignments(t, ctx, 5*time.Minute)

	for _, a := range first {
		if a.MessageID <= 0 {
			t.Errorf("message id %d is not positive; the delivery key and the durable "+
				"cursor both rest on it", a.MessageID)
		}
		if a.JobID == "" {
			t.Errorf("assignment for run %d carries no job id; the attempt key rests on it", a.RunID)
		}
		// Recorded, not asserted: the schema stores this as a diagnostic
		// and zero is a legitimate value in a flow where announcing
		// capacity is what admits the job.
		t.Logf("runner_request_id for job %s: %d", a.JobID, a.RequestID)
	}

	// The message was never acknowledged, so polling from the same
	// cursor must hand back the same delivery.
	second := f.awaitAssignments(t, ctx, 2*time.Minute)
	if len(second) != len(first) {
		t.Fatalf("redelivery carried %d assignments; the first delivery carried %d",
			len(second), len(first))
	}
	if second[0].MessageID != first[0].MessageID {
		t.Errorf("redelivered message id = %d; want %d — an unacknowledged message must "+
			"come back under the same id or the ack state machine has no cursor",
			second[0].MessageID, first[0].MessageID)
	}
	if got, want := fingerprints(second), fingerprints(first); got != want {
		t.Errorf("redelivery content changed:\n got %s\nwant %s\n"+
			"the same natural key with different content is contract drift, and the "+
			"product stops the binding rather than overwriting", got, want)
	}

	// Acknowledged only now, so the fixture does not leave the message
	// pending for the next run.
	if err := f.session.DeleteMessage(ctx, second[0].MessageID); err != nil {
		t.Errorf("acknowledge the observed message: %v", err)
	}
}

// TestLapsedAssignmentIsCancelledAndRequeued measures what upstream
// documents: an assignment no runner acquires in time is cancelled and
// the job requeued, the cancellation arriving as a JobCompleted with
// result "canceled".
//
// The requeue is asserted, having been observed. The cancellation is
// recorded and not asserted — which identity it carries decides how the
// product must close the attempt the lapsed assignment left behind, and
// a gate cannot demand a fact nobody has measured. The evidence artifact
// is the deliverable of this test as much as its verdict.
//
// It is slow by nature — it waits for GitHub's own timer — so outside
// release qualification it is opt-in even when the rest of the suite
// runs.
func TestLapsedAssignmentIsCancelledAndRequeued(t *testing.T) {
	if os.Getenv("RUNPOOL_CONTRACT_REASSIGNMENT") == "" && !qualifying() {
		t.Skip("set RUNPOOL_CONTRACT_REASSIGNMENT=1; this test waits for GitHub to lapse an assignment")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Minute)
	defer cancel()
	f := newIdentityFixture(t, ctx)
	f.dispatch(t, ctx)

	first := f.awaitAssignments(t, ctx, 5*time.Minute)
	original := first[0]

	// Acknowledge and then never serve it: the assignment is accepted
	// and no runner ever claims the job, which is what makes the
	// provider lapse it.
	f.cursor = original.MessageID
	if err := f.session.DeleteMessage(ctx, original.MessageID); err != nil {
		t.Fatalf("acknowledge the first assignment: %v", err)
	}

	// Both halves may arrive in either order, or together, so the
	// predicate accumulates across messages rather than matching one.
	var cancelled, requeued *observedJob
	f.await(t, ctx, 12*time.Minute, func(m observedMessage) bool {
		for i, j := range m.Jobs {
			switch {
			case j.Kind == kindCompleted && j.JobID == original.JobID:
				cancelled = &m.Jobs[i]
			case j.Kind == kindAssigned && j.JobID != original.JobID && j.RunID == original.RunID:
				requeued = &m.Jobs[i]
			}
		}
		return cancelled != nil && requeued != nil
	})

	if requeued == nil {
		t.Fatalf("the job of run %d was never requeued after its assignment lapsed; "+
			"the provider documents a requeue and the product's recovery assumes one",
			original.RunID)
	}
	t.Logf("requeued under a different job id: %s -> %s (run %d, message %d -> %d, "+
		"assigned %s -> %s)", original.JobID, requeued.JobID, original.RunID,
		original.MessageID, requeued.MessageID,
		original.ScaleSetAssignTime.Format(time.RFC3339), requeued.ScaleSetAssignTime.Format(time.RFC3339))

	// Recorded, not asserted. Whether this arrives at all, and under
	// which job id, is exactly what the product's cancelIfReady keys on.
	switch {
	case cancelled == nil:
		t.Logf("no JobCompleted for the lapsed job %s reached this session within the window",
			original.JobID)
	default:
		t.Logf("lapsed job %s completed with result %q under message %d",
			cancelled.JobID, cancelled.Result, cancelled.MessageID)
	}

	if requeued.MessageID != original.MessageID {
		if err := f.session.DeleteMessage(ctx, requeued.MessageID); err != nil {
			t.Errorf("acknowledge the requeue: %v", err)
		}
	}
}

func fingerprints(as []observedJob) string {
	out := make([]string, len(as))
	for i, a := range as {
		out[i] = a.Fingerprint
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}
