package githubcontract

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/actions/scaleset"
)

const (
	envRepoA     = "RUNPOOL_CONTRACT_REPO_A"     // owner/name of the first org repository
	envRepoB     = "RUNPOOL_CONTRACT_REPO_B"     // owner/name of the second org repository
	envRunnerCmd = "RUNPOOL_CONTRACT_RUNNER_CMD" // command that starts a JIT runner; jitconfig on stdin, runner name appended

	organizationProbeWorkflow    = "organization-cache.yml"
	organizationProbeFixturePath = ".github/workflows/organization-cache.yml"
)

// TestOrganizationJitAssignmentNotBound is the live proof that an
// organization-scoped JIT runner is not bound to a repository when it is
// provisioned.
//
// Broker contract observed live (v0.4.0, GitHub.com): while the session
// announces free capacity, a queued job arrives directly as JobAssigned
// with runnerRequestId=0 — no JobAvailable, no AcquireJobs, no local
// per-job acceptance step. Announcing capacity IS accepting work, which
// is why the capacity allocator must govern maxCapacity.
//
// The not-bound proof is a symmetric crossover. Repository B's job is
// assigned first, then A's. Two JIT runners start in the opposite
// nominal order — "nominal-a" first, then "nominal-b", each the runner a
// naive autoscaler would have provisioned (with that repository's cache)
// for the demand message it reacted to. JIT generation takes only a name
// and work folder, so if runner nominal-a executes B's job and nominal-b
// executes A's, repository metadata demonstrably does not bind a runner,
// in either direction. This is why organization-scoped scale sets must
// not mount repository caches.
//
// Requirements beyond the organization credential: both repositories carry
// the byte-identical versioned probe workflow, and
// RUNPOOL_CONTRACT_RUNNER_CMD starts a runner container on a Docker host. The
// command receives the JIT bundle on stdin. Its implementation must not log it
// or place it in host-persistent state; the upstream runner ultimately consumes
// it through its required --jitconfig argument inside the disposable runner.
func TestOrganizationJitAssignmentNotBound(t *testing.T) {
	url, token := target(t, envOrgURL, envOrgToken)
	repoA, repoB, runnerCmd := os.Getenv(envRepoA), os.Getenv(envRepoB), os.Getenv(envRunnerCmd)
	if repoA == "" || repoB == "" || runnerCmd == "" {
		// This contract is the whole basis of the organization-scope
		// cache rule: a JIT runner is not bound to the job that caused
		// it, so a repository cache cannot be chosen before the runner
		// knows which repository it got. Qualifying without running it
		// would attest a rule nothing checked.
		if os.Getenv(envQualify) != "" {
			t.Fatalf("release qualification requires %s, %s and %s; the organization assignment contract cannot be skipped",
				envRepoA, envRepoB, envRunnerCmd)
		}
		t.Skipf("%s, %s and %s not set; the organization assignment contract is opt-in", envRepoA, envRepoB, envRunnerCmd)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()
	c := newClient(t, url, token)
	rest := newRESTClient(token)
	verifyOrganizationProbeFixtures(t, ctx, rest, repoA, repoB)

	group, err := c.GetRunnerGroupByName(ctx, scaleset.DefaultRunnerGroup)
	if err != nil {
		t.Fatalf("resolve Default runner group: %v", err)
	}

	label := "runpool-org-" + randomHex(t, 5)
	existing, err := c.GetRunnerScaleSet(ctx, group.ID, label)
	if err != nil {
		t.Fatalf("check the scale set name is free: %v", err)
	}
	if existing != nil {
		t.Fatalf("scale set %q already exists; refusing to adopt a foreign fixture", label)
	}
	set, err := c.CreateRunnerScaleSet(ctx, &scaleset.RunnerScaleSet{
		Name:          label,
		RunnerGroupID: group.ID,
	})
	if err != nil {
		t.Fatalf("create scale set: %v", err)
	}
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()
		if err := c.DeleteRunnerScaleSet(cctx, set.ID); err != nil {
			t.Errorf("cleanup: delete scale set %d: %v", set.ID, err)
		}
	})

	// Open the listener before dispatch so no assignment can precede the
	// observed cursor.
	session := openContractSession(t, ctx, c, set.ID, "runpool-organization-contract")

	poller := &messagePoller{session: session}
	suffix := uniqueName(t)[len("runpool-ct-"):]

	runB := dispatchOrganizationProbe(t, ctx, rest, repoB, label)
	t.Cleanup(func() { cancelProbeRun(t, rest, repoB, runB.ID) })
	assignedB := poller.waitAssigned(t, ctx, repoB)
	t.Logf("repository identity %s/%s known before provisioning (assigned from announced capacity)",
		assignedB.OwnerName, assignedB.RepositoryName)

	runA := dispatchOrganizationProbe(t, ctx, rest, repoA, label)
	t.Cleanup(func() { cancelProbeRun(t, rest, repoA, runA.ID) })
	assignedA := poller.waitAssigned(t, ctx, repoA)
	t.Logf("both jobs now assigned to the scale set: %s then %s, no AcquireJobs involved",
		assignedB.RepositoryName, assignedA.RepositoryName)

	// The naive reaction to A's message: provision a runner "for A".
	nominalA := "cache-probe-a-" + suffix
	startJitRunner(t, ctx, c, set.ID, runnerCmd, nominalA)

	started1 := poller.waitStarted(t, ctx)
	t.Logf("JobStarted: runner %q executes %s/%s", started1.RunnerName, started1.OwnerName, started1.RepositoryName)
	if started1.RunnerName != nominalA {
		t.Fatalf("first job started on %q; want %q", started1.RunnerName, nominalA)
	}
	if got := started1.OwnerName + "/" + started1.RepositoryName; got != repoB {
		t.Fatalf("runner nominally for A executed %s; the crossover proof expects %s", got, repoB)
	}

	nominalB := "cache-probe-b-" + suffix
	startJitRunner(t, ctx, c, set.ID, runnerCmd, nominalB)

	started2 := poller.waitStarted(t, ctx)
	t.Logf("JobStarted: runner %q executes %s/%s", started2.RunnerName, started2.OwnerName, started2.RepositoryName)
	if started2.RunnerName != nominalB {
		t.Fatalf("second job started on %q; want %q", started2.RunnerName, nominalB)
	}
	if got := started2.OwnerName + "/" + started2.RepositoryName; got != repoA {
		t.Fatalf("runner nominally for B executed %s; the crossover proof expects %s", got, repoA)
	}

	// Completion truth comes from the runner exiting and the REST run
	// state. Live finding: JobCompleted messages may arrive very late or
	// not at all while the session idles (observed: successful jobs, six
	// silent minutes) — hard evidence for the design rule that cleanup
	// is triggered by runner exit, hints, timeout and reconciliation,
	// never one signal alone.
	awaitRunConclusion(t, ctx, rest, repoB, assignedB.WorkflowRunID)
	awaitRunConclusion(t, ctx, rest, repoA, assignedA.WorkflowRunID)
	poller.drainCompletions(t, 20*time.Second)
	t.Log("crossover complete in both directions: repository metadata does not bind a JIT runner")
}

// awaitRunConclusion polls the REST state until the run concludes.
func awaitRunConclusion(t *testing.T, ctx context.Context, rest *restClient, repo string, runID int64) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for {
		run, err := rest.workflowRun(ctx, repo, runID)
		if err == nil && run.Status == "completed" && run.Conclusion == "success" {
			t.Logf("run %d on %s: completed/success", runID, repo)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %d on %s did not conclude successfully: %s/%s, %v",
				runID, repo, run.Status, run.Conclusion, err)
		}
		time.Sleep(5 * time.Second)
	}
}

func startJitRunner(t *testing.T, ctx context.Context, c *scaleset.Client, setID int, command, name string) {
	t.Helper()
	jit, err := c.GenerateJitRunnerConfig(ctx,
		&scaleset.RunnerScaleSetJitRunnerSetting{Name: name, WorkFolder: "_work"}, setID)
	if err != nil {
		t.Fatalf("generate jit config for %q: %v", name, err)
	}
	parts := append(strings.Fields(command), name)
	cmd := exec.CommandContext(ctx, parts[0], parts[1:]...)
	cmd.Stdin = strings.NewReader(jit.EncodedJITConfig)
	out, err := cmd.CombinedOutput()
	if strings.Contains(string(out), jit.EncodedJITConfig) {
		t.Fatal("runner starter copied the JIT bundle to its output; refusing to retain the log")
	}
	if err != nil {
		t.Fatalf("start runner %q: %v: %s", name, err, out)
	}
	t.Logf("runner starter: %s", strings.TrimSpace(string(out)))
}

// messagePoller drains the session stream, acknowledging every message
// and remembering the last id, so each wait sees fresh messages only.
type messagePoller struct {
	session *scaleset.MessageSessionClient
	lastID  int
}

func (p *messagePoller) poll(t *testing.T, ctx context.Context, handle func(*scaleset.RunnerScaleSetMessage) bool) {
	t.Helper()
	for {
		if ctx.Err() != nil {
			t.Fatalf("timed out waiting for messages: %v", ctx.Err())
		}
		msg, err := p.session.GetMessage(ctx, p.lastID, 2)
		if err != nil {
			t.Fatalf("get message: %v", err)
		}
		if msg == nil {
			continue // empty long poll
		}
		p.lastID = msg.MessageID
		done := handle(msg)
		if err := p.session.DeleteMessage(ctx, msg.MessageID); err != nil {
			t.Fatalf("delete message %d: %v", msg.MessageID, err)
		}
		if done {
			return
		}
	}
}

func (p *messagePoller) waitAssigned(t *testing.T, ctx context.Context, repo string) *scaleset.JobAssigned {
	t.Helper()
	owner, name, _ := strings.Cut(repo, "/")
	var found *scaleset.JobAssigned
	p.poll(t, ctx, func(msg *scaleset.RunnerScaleSetMessage) bool {
		for _, j := range msg.JobAvailableMessages {
			t.Logf("JobAvailable (unexpected in this flow): %s/%s request=%d", j.OwnerName, j.RepositoryName, j.RunnerRequestID)
		}
		for _, j := range msg.JobAssignedMessages {
			t.Logf("JobAssigned: %s/%s run=%d", j.OwnerName, j.RepositoryName, j.WorkflowRunID)
			if j.OwnerName == owner && j.RepositoryName == name {
				found = j
			}
		}
		return found != nil
	})
	return found
}

func (p *messagePoller) waitStarted(t *testing.T, ctx context.Context) *scaleset.JobStarted {
	t.Helper()
	var found *scaleset.JobStarted
	p.poll(t, ctx, func(msg *scaleset.RunnerScaleSetMessage) bool {
		if len(msg.JobStartedMessages) > 0 {
			found = msg.JobStartedMessages[0]
		}
		return found != nil
	})
	return found
}

// drainCompletions observes late lifecycle messages briefly and without
// failing: they are hints, not the completion authority.
func (p *messagePoller) drainCompletions(t *testing.T, window time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), window)
	defer cancel()
	for {
		msg, err := p.session.GetMessage(ctx, p.lastID, 2)
		if err != nil {
			if ctx.Err() == nil {
				t.Errorf("observe late completion messages: %v", err)
			}
			return
		}
		if msg == nil {
			return
		}
		for _, j := range msg.JobCompletedMessages {
			t.Logf("late JobCompleted: %s/%s on %q result=%q", j.OwnerName, j.RepositoryName, j.RunnerName, j.Result)
		}
		if err := p.session.DeleteMessage(ctx, msg.MessageID); err != nil {
			if ctx.Err() == nil {
				t.Errorf("acknowledge late completion message %d: %v", msg.MessageID, err)
			}
			return
		}
		p.lastID = msg.MessageID
	}
}

func verifyOrganizationProbeFixtures(t *testing.T, ctx context.Context, rest *restClient, repos ...string) {
	t.Helper()
	fixture, err := os.ReadFile(filepath.Join("testdata", organizationProbeWorkflow))
	if err != nil {
		t.Fatalf("read the organization probe fixture: %v", err)
	}
	want := sha256.Sum256(fixture)
	for _, repo := range repos {
		got, err := rest.installedFixtureDigest(ctx, repo, organizationProbeFixturePath)
		if err != nil {
			t.Fatalf("read the organization probe installed on %s: %v", repo, err)
		}
		if got != hex.EncodeToString(want[:]) {
			t.Fatalf("the installed %s on %s differs from testdata/%s; install the versioned fixture",
				organizationProbeFixturePath, repo, organizationProbeWorkflow)
		}
	}
}

func dispatchOrganizationProbe(t *testing.T, ctx context.Context, rest *restClient, repo, runnerLabel string) workflowRun {
	t.Helper()
	correlation := randomHex(t, 6)
	if err := rest.dispatchWorkflow(ctx, repo, organizationProbeWorkflow,
		map[string]string{"correlation_id": correlation, "runner_label": runnerLabel}); err != nil {
		t.Fatalf("dispatch %s on %s: %v", organizationProbeWorkflow, repo, err)
	}
	run, err := rest.findRunByCorrelation(ctx, repo, organizationProbeWorkflow, correlation, time.Minute)
	if err != nil {
		t.Fatalf("locate %s on %s: %v", organizationProbeWorkflow, repo, err)
	}
	t.Logf("dispatched %s on %s: run %d", organizationProbeWorkflow, repo, run.ID)
	return run
}

// cancelProbeRun cancels only the run created by this contract.
func cancelProbeRun(t *testing.T, rest *restClient, repo string, runID int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := rest.cancelRun(ctx, repo, runID); err != nil {
		t.Errorf("cleanup: cancel run %d on %s: %v", runID, repo, err)
	}
}
