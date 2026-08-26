// Package controllere2e drives a real provider assignment through the released
// controller and capsule candidates. It is opt-in because it creates protected
// GitHub fixture resources and Docker objects on a dedicated host.
package controllere2e

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	envEnabled         = "RUNPOOL_CONTROLLER_E2E"
	envQualify         = "RUNPOOL_CONTRACT_QUALIFY"
	envControllerImage = "RUNPOOL_E2E_CONTROLLER_IMAGE"
	envCapsuleImage    = "RUNPOOL_E2E_CAPSULE_IMAGE"
	envRepository      = "RUNPOOL_E2E_REPOSITORY"
	envGitRevision     = "RUNPOOL_E2E_GIT_REVISION"
	envToken           = "RUNPOOL_E2E_TOKEN"
	envArtifactDir     = "RUNPOOL_E2E_ARTIFACT_DIR"
	envAPIURL          = "RUNPOOL_E2E_API_URL"
	envDockerSocket    = "RUNPOOL_E2E_DOCKER_SOCKET"
)

type settings struct {
	controllerImage string
	capsuleImage    string
	repository      string
	owner           string
	repositoryName  string
	gitRevision     string
	token           string
	artifactDir     string
	apiURL          string
	dockerSocket    string
	runID           string
	runnerLabel     string
}

type qualificationEvidence struct {
	StartedAt      time.Time          `json:"started_at"`
	CompletedAt    time.Time          `json:"completed_at"`
	Repository     string             `json:"repository"`
	Controller     string             `json:"controller_image"`
	Capsule        string             `json:"capsule_image"`
	RunnerLabel    string             `json:"runner_label"`
	CacheKey       string             `json:"cache_key"`
	WorkflowRuns   []workflowEvidence `json:"workflow_runs"`
	Cleanup        map[string]string  `json:"cleanup"`
	Outcome        string             `json:"outcome"`
	FailureSummary string             `json:"failure_summary,omitempty"`
}

type workflowEvidence struct {
	Purpose string `json:"purpose"`
	ID      int64  `json:"id"`
	URL     string `json:"url"`
}

func TestControllerEndToEnd(t *testing.T) {
	s := loadSettings(t)
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Minute)
	defer cancel()

	fixture, err := os.ReadFile("testdata/workload.yml")
	if err != nil {
		t.Fatal(err)
	}
	github := newGitHubClient(s)
	if err := github.verifyFixture(ctx, s.gitRevision, fixture); err != nil {
		t.Fatal(err)
	}

	evidence := qualificationEvidence{
		StartedAt:   time.Now().UTC(),
		Repository:  s.repository,
		Controller:  s.controllerImage,
		Capsule:     s.capsuleImage,
		RunnerLabel: s.runnerLabel,
		CacheKey:    "marker-" + s.runID,
		Cleanup:     map[string]string{},
		Outcome:     "incomplete",
	}
	docker := newDockerHarness(s, t.TempDir())
	var runs []workflowRun
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 8*time.Minute)
		defer cleanupCancel()
		for _, run := range runs {
			current, getErr := github.getRun(cleanupCtx, run.ID)
			if getErr == nil && current.Status == "completed" {
				// A completed run removed its own image: the workflow's
				// final step deletes the version it pushed with the run's
				// own token. That works on the user-scoped packages route
				// and only there -- the org-scoped one answers a workflow
				// token with 400 -- which is why the workflow spells the
				// endpoint the way it does. The run's log is the proof.
				continue
			}
			if getErr == nil {
				if cancelErr := github.cancel(cleanupCtx, run.ID); cancelErr != nil {
					t.Errorf("cleanup workflow %d: %v", run.ID, cancelErr)
				}
			}
			tag, ok := strings.CutPrefix(run.DisplayName, "runpool-e2e-")
			if !ok {
				t.Errorf("workflow %d has unexpected display title %q", run.ID, run.DisplayName)
				continue
			}
			// A run that never completed may not have reached its cleanup
			// step, and this credential cannot look: the packages API
			// refuses installation tokens. The tag is named in the
			// evidence for the maintainer sweep.
			evidence.Cleanup["ghcr_image_"+tag] = "run did not complete; the version, if pushed, outlives the run"
			t.Logf("GHCR image %q may outlive the cancelled run %d", tag, run.ID)
		}
		cleanup, cleanupErr := docker.cleanup(cleanupCtx)
		for key, value := range cleanup {
			evidence.Cleanup[key] = value
		}
		if cleanupErr != nil {
			t.Errorf("Docker cleanup: %v", cleanupErr)
		}
		evidence.CompletedAt = time.Now().UTC()
		if t.Failed() {
			evidence.Outcome = "failed"
			evidence.FailureSummary = "see the linked workflow run and redacted job logs"
		} else {
			evidence.Outcome = "passed"
		}
		if err := writeEvidence(s.artifactDir, s.runID, evidence); err != nil {
			t.Errorf("write E2E evidence: %v", err)
		}
	})

	if err := docker.prepare(ctx); err != nil {
		t.Fatal(err)
	}
	if err := docker.start(ctx, "primary-"+s.runID); err != nil {
		t.Fatal(err)
	}

	seed := dispatch(t, ctx, github, s, evidence.CacheKey, "seed", 90)
	runs = append(runs, seed)
	if _, err := github.waitForStatus(ctx, seed, "in_progress", 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := docker.restart(ctx); err != nil {
		t.Fatal(err)
	}
	seed = awaitSuccess(t, ctx, github, seed)
	evidence.WorkflowRuns = append(evidence.WorkflowRuns, workflowEvidence{Purpose: "restart_and_seed", ID: seed.ID, URL: seed.HTMLURL})

	reuse := dispatch(t, ctx, github, s, evidence.CacheKey, "reuse", 0)
	runs = append(runs, reuse)
	reuse = awaitSuccess(t, ctx, github, reuse)
	evidence.WorkflowRuns = append(evidence.WorkflowRuns, workflowEvidence{Purpose: "cache_reuse", ID: reuse.ID, URL: reuse.HTMLURL})

	if err := docker.switchGeneration(ctx, "isolated-"+s.runID); err != nil {
		t.Fatal(err)
	}
	isolation := dispatch(t, ctx, github, s, evidence.CacheKey, "isolated", 0)
	runs = append(runs, isolation)
	isolation = awaitSuccess(t, ctx, github, isolation)
	evidence.WorkflowRuns = append(evidence.WorkflowRuns, workflowEvidence{Purpose: "cache_generation_isolation", ID: isolation.ID, URL: isolation.HTMLURL})
}

func loadSettings(t *testing.T) settings {
	t.Helper()
	if os.Getenv(envEnabled) == "" {
		if os.Getenv(envQualify) != "" {
			t.Fatalf("%s is required during release qualification", envEnabled)
		}
		t.Skipf("set %s=1 to run the controller E2E suite", envEnabled)
	}
	require := func(name string) string {
		t.Helper()
		value := os.Getenv(name)
		if value == "" {
			t.Fatalf("%s is required", name)
		}
		return value
	}
	s := settings{
		controllerImage: require(envControllerImage),
		capsuleImage:    require(envCapsuleImage),
		repository:      require(envRepository),
		token:           require(envToken),
		artifactDir:     require(envArtifactDir),
		gitRevision:     valueOrDefault(envGitRevision, "main"),
		apiURL:          valueOrDefault(envAPIURL, "https://api.github.com"),
		dockerSocket:    valueOrDefault(envDockerSocket, "/var/run/docker.sock"),
		runID:           randomHex(t, 6),
	}
	parts := strings.Split(s.repository, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		t.Fatalf("%s must be owner/repository, got %q", envRepository, s.repository)
	}
	s.owner, s.repositoryName = parts[0], parts[1]
	s.runnerLabel = "runpool-e2e-" + s.runID
	for _, image := range []struct {
		name  string
		value string
	}{{envControllerImage, s.controllerImage}, {envCapsuleImage, s.capsuleImage}} {
		if !strings.Contains(image.value, "@sha256:") {
			t.Fatalf("%s must be digest-qualified, got %q", image.name, image.value)
		}
	}
	return s
}

func dispatch(t *testing.T, ctx context.Context, github *githubClient, s settings, cacheKey, expectation string, hold int) workflowRun {
	t.Helper()
	runID := s.runID + "-" + expectation
	run, err := github.dispatch(ctx, s.gitRevision, runID, s.runnerLabel, cacheKey, expectation, hold)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func awaitSuccess(t *testing.T, ctx context.Context, github *githubClient, run workflowRun) workflowRun {
	t.Helper()
	completed, err := github.waitForSuccess(ctx, run, 20*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return completed
}

func writeEvidence(dir, runID string, evidence qualificationEvidence) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	payload, err := json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "controller-e2e-"+runID+".json"), payload, 0o600)
}

func randomHex(t *testing.T, bytes int) string {
	t.Helper()
	payload := make([]byte, bytes)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(payload)
}

func valueOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
