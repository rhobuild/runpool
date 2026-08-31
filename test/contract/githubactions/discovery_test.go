package githubcontract

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/actions/scaleset"

	"github.com/rhobuild/runpool/internal/allocator"
	"github.com/rhobuild/runpool/internal/assignment"
)

const (
	discoveryTier        assignment.TierID = "contract"
	discoveryParallelism int               = 1
)

type discoveryStep struct {
	Binding   string `json:"binding"`
	Capacity  int    `json:"capacity"`
	MessageID int    `json:"message_id,omitempty"`
	Empty     bool   `json:"empty"`
	Assigned  bool   `json:"assigned"`
}

type discoveryEvidence struct {
	SchemaVersion int             `json:"schema_version"`
	CapacityLimit int             `json:"capacity_limit"`
	QueuedBinding string          `json:"queued_binding"`
	DiscoveredBy  string          `json:"discovered_by"`
	WorkflowRunID int64           `json:"workflow_run_id"`
	StartedAt     time.Time       `json:"started_at"`
	CompletedAt   time.Time       `json:"completed_at"`
	MaximumRemote int             `json:"maximum_remote_capacity"`
	Steps         []discoveryStep `json:"steps"`
	Outcome       string          `json:"outcome"`
}

// TestDiscoveryCreditReachesASilentBinding runs the production allocator
// against two real scale-set sessions. Work is queued only for the binding
// that does not initially hold the single discovery credit; the test requires
// the first holder to observe an empty poll, revoke its credit, and the second
// holder to discover the assignment without ever advertising more than one.
func TestDiscoveryCreditReachesASilentBinding(t *testing.T) {
	url, token := target(t, envRepoURL, envRepoToken)
	repo := repoSlug(t, url)
	client := newClient(t, url, token)
	rest := newRESTClient(token)

	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Minute)
	defer cancel()
	verifyOrganizationProbeFixtures(t, ctx, rest, repo)

	group, err := client.GetRunnerGroupByName(ctx, scaleset.DefaultRunnerGroup)
	if err != nil {
		t.Fatalf("resolve the default runner group: %v", err)
	}
	firstName, secondName := uniqueName(t), uniqueName(t)
	firstSet := createScaleSet(t, client, group, firstName)
	secondSet := createScaleSet(t, client, group, secondName)
	firstSession := openContractSession(t, ctx, client, firstSet.ID, "runpool-discovery-first")
	secondSession := openContractSession(t, ctx, client, secondSet.ID, "runpool-discovery-second")

	credit := allocator.New()
	for _, key := range []assignment.BindingKey{assignment.BindingKey(firstName), assignment.BindingKey(secondName)} {
		if err := credit.Register(discoveryTier, key, discoveryParallelism); err != nil {
			t.Fatalf("register binding %q: %v", key, err)
		}
		credit.SessionOpened(key)
	}

	evidence := discoveryEvidence{
		SchemaVersion: 1, CapacityLimit: discoveryParallelism, QueuedBinding: secondName,
		StartedAt: time.Now().UTC(), Outcome: "incomplete",
	}
	defer func() {
		evidence.CompletedAt = time.Now().UTC()
		if t.Failed() {
			evidence.Outcome = "failed"
		} else {
			evidence.Outcome = "passed"
		}
		writeDiscoveryEvidence(t, evidence)
	}()

	run := dispatchOrganizationProbe(t, ctx, rest, repo, secondName)
	evidence.WorkflowRunID = run.ID
	t.Cleanup(func() { cancelProbeRun(t, rest, repo, run.ID) })

	cursors := map[assignment.BindingKey]int{
		assignment.BindingKey(firstName):  0,
		assignment.BindingKey(secondName): 0,
	}
	sessions := map[assignment.BindingKey]*scaleset.MessageSessionClient{
		assignment.BindingKey(firstName):  firstSession,
		assignment.BindingKey(secondName): secondSession,
	}

	first := assignment.BindingKey(firstName)
	second := assignment.BindingKey(secondName)
	pollUntilEmpty(t, ctx, credit, first, sessions[first], cursors, &evidence)
	if holder := credit.DiscoveryHolder(discoveryTier); holder != second {
		t.Fatalf("empty poll left discovery on %q; want %q", holder, second)
	}
	pollOnceWithAllocator(t, ctx, credit, first, sessions[first], cursors, &evidence)
	if got := evidence.Steps[len(evidence.Steps)-1].Capacity; got != 0 {
		t.Fatalf("previous holder %q announced %d while revoking; want zero", first, got)
	}

	deadline := time.Now().Add(2 * time.Minute)
	for evidence.DiscoveredBy == "" && time.Now().Before(deadline) {
		pollOnceWithAllocator(t, ctx, credit, second, sessions[second], cursors, &evidence)
	}
	if evidence.DiscoveredBy != secondName {
		t.Fatalf("queued run %d was not discovered by %q", run.ID, secondName)
	}
	if evidence.MaximumRemote > discoveryParallelism {
		t.Fatalf("possible remote capacity reached %d with a limit of %d", evidence.MaximumRemote, discoveryParallelism)
	}
	t.Logf("discovery run %d was found by %s after %d broker polls; maximum possible remote capacity=%d",
		run.ID, evidence.DiscoveredBy, len(evidence.Steps), evidence.MaximumRemote)
}

func pollUntilEmpty(
	t *testing.T,
	ctx context.Context,
	credit *allocator.Allocator,
	key assignment.BindingKey,
	session *scaleset.MessageSessionClient,
	cursors map[assignment.BindingKey]int,
	evidence *discoveryEvidence,
) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		pollOnceWithAllocator(t, ctx, credit, key, session, cursors, evidence)
		if evidence.Steps[len(evidence.Steps)-1].Empty {
			return
		}
	}
	t.Fatalf("binding %q did not complete an empty discovery poll", key)
}

func pollOnceWithAllocator(
	t *testing.T,
	ctx context.Context,
	credit *allocator.Allocator,
	key assignment.BindingKey,
	session *scaleset.MessageSessionClient,
	cursors map[assignment.BindingKey]int,
	evidence *discoveryEvidence,
) {
	t.Helper()
	poll := credit.BeginPoll(key)
	if !poll.Valid() {
		t.Fatalf("allocator refused a poll for registered binding %q", key)
	}
	pollCtx, cancel := context.WithTimeout(ctx, 75*time.Second)
	msg, err := session.GetMessage(pollCtx, cursors[key], poll.Capacity())
	cancel()
	if err != nil {
		credit.CompletePoll(poll, false, false)
		t.Fatalf("poll binding %q at capacity %d: %v", key, poll.Capacity(), err)
	}
	step := discoveryStep{Binding: string(key), Capacity: poll.Capacity(), Empty: msg == nil}
	if msg != nil {
		step.MessageID = msg.MessageID
		for _, job := range msg.JobAssignedMessages {
			if job.WorkflowRunID != evidence.WorkflowRunID {
				t.Fatalf("binding %q received unexpected workflow run %d; want %d", key, job.WorkflowRunID, evidence.WorkflowRunID)
			}
			step.Assigned = true
		}
		if statistics := msg.Statistics; statistics != nil {
			credit.SetAssignedDemand(key, statistics.TotalAssignedJobs)
		}
		if err := session.DeleteMessage(ctx, msg.MessageID); err != nil {
			credit.CompletePoll(poll, false, false)
			t.Fatalf("acknowledge message %d for binding %q: %v", msg.MessageID, key, err)
		}
		cursors[key] = msg.MessageID
		if step.Assigned {
			evidence.DiscoveredBy = string(key)
		}
	}
	credit.CompletePoll(poll, true, msg == nil)
	evidence.Steps = append(evidence.Steps, step)
	if total := possibleRemoteCapacity(credit, discoveryTier); total > evidence.MaximumRemote {
		evidence.MaximumRemote = total
	}
}

func possibleRemoteCapacity(credit *allocator.Allocator, tier assignment.TierID) int {
	_, rows := credit.PoolReport(tier)
	total := 0
	for _, row := range rows {
		total += row.RemoteCapacity
	}
	return total
}

func writeDiscoveryEvidence(t *testing.T, evidence discoveryEvidence) {
	t.Helper()
	directory := os.Getenv("RUNPOOL_CONTRACT_ARTIFACT_DIR")
	if directory == "" {
		if qualifying() {
			t.Error("RUNPOOL_CONTRACT_ARTIFACT_DIR is unset during release qualification")
		}
		return
	}
	payload, err := json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		t.Errorf("encode discovery evidence: %v", err)
		return
	}
	name := fmt.Sprintf("discovery-%s.json", evidence.StartedAt.Format("20060102T150405Z"))
	if err := os.WriteFile(filepath.Join(directory, name), payload, 0o644); err != nil {
		t.Errorf("write discovery evidence: %v", err)
	}
}
