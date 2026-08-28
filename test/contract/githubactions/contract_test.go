// Package githubcontract is the live contract suite for the pinned
// github.com/actions/scaleset client. It verifies organization and repository
// scale sets, JIT runner lifecycle, delivery identity, capacity semantics, and
// authentication failures against explicitly authorized fixtures.
//
// The tests are gated by environment and run against real, explicitly
// authorized GitHub targets. Each pair of variables enables its scope:
//
//	RUNPOOL_CONTRACT_ORG_URL   RUNPOOL_CONTRACT_ORG_TOKEN
//	RUNPOOL_CONTRACT_REPO_URL  RUNPOOL_CONTRACT_REPO_TOKEN
//
// Tokens need self-hosted-runner administration on their target. Every
// created resource has a unique run-scoped name and is deleted by
// cleanup, so an aborted run leaves at most an empty scale set behind.
package githubcontract

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/actions/scaleset"

	"github.com/rhobuild/runpool/internal/credential"
	"github.com/rhobuild/runpool/internal/platform/githubactions"
)

const (
	envOrgURL    = "RUNPOOL_CONTRACT_ORG_URL"
	envOrgToken  = "RUNPOOL_CONTRACT_ORG_TOKEN"
	envRepoURL   = "RUNPOOL_CONTRACT_REPO_URL"
	envRepoToken = "RUNPOOL_CONTRACT_REPO_TOKEN"
	// envQualify turns every skip in this suite into a failure.
	envQualify = "RUNPOOL_CONTRACT_QUALIFY"
)

func target(t *testing.T, urlVar, tokenVar string) (url, token string) {
	t.Helper()
	url, token = os.Getenv(urlVar), os.Getenv(tokenVar)
	if url == "" || token == "" {
		if os.Getenv(envQualify) != "" {
			t.Fatalf("release qualification requires %s and %s; the contract cannot be skipped", urlVar, tokenVar)
		}
		t.Skipf("%s and %s not set; live GitHub contract tests are opt-in", urlVar, tokenVar)
	}
	return url, token
}

// newClient records the pinned client version in the test log so every
// contract run states exactly which dependency version it exercised.
func newClient(t *testing.T, configURL, token string) *scaleset.Client {
	t.Helper()
	c, err := scaleset.NewClientWithPersonalAccessToken(scaleset.NewClientWithPersonalAccessTokenConfig{
		GitHubConfigURL:     configURL,
		PersonalAccessToken: token,
		SystemInfo: scaleset.SystemInfo{
			System:    "runpool",
			Version:   clientModuleVersion(),
			Subsystem: "contract-test",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("actions/scaleset %s against %s", clientModuleVersion(), configURL)
	return c
}

// clientModuleVersion reads the pin from go.mod: test binaries carry no
// dependency list in their build info, and the module file is the source
// of truth anyway. Contract tests always run from the repository, so the
// relative path holds.
func clientModuleVersion() string {
	data, err := os.ReadFile("../../../go.mod")
	if err != nil {
		return "unknown"
	}
	for _, line := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "github.com/actions/scaleset "); ok {
			return strings.TrimSpace(v)
		}
	}
	return "unknown"
}

// uniqueName returns a run-scoped scale-set name so concurrent or
// aborted runs never collide with each other or with real scale sets.
func uniqueName(t *testing.T) string {
	t.Helper()
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "runpool-ct-" + hex.EncodeToString(b)
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// newWrapper builds the production adapter the org/repo tests exercise,
// so internal/platform/githubactions is qualified by the same live suite
// that pins the upstream contract.
func newWrapper(t *testing.T, configURL, token string) *githubactions.Client {
	t.Helper()
	gh, err := githubactions.NewClient(githubactions.ClientConfig{
		ConfigURL:  configURL,
		Credential: credential.Secret{Token: token},
		Version:    "contract-test/" + clientModuleVersion(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("actions/scaleset %s against %s", clientModuleVersion(), configURL)
	return gh
}

// ensureSet ensures a scale set through the wrapper and guarantees its
// deletion, even when the test fails midway.
func ensureSet(t *testing.T, gh *githubactions.Client, name string) githubactions.ScaleSet {
	t.Helper()
	intent := &intentRecorder{}
	set, err := gh.EnsureScaleSet(testCtx(t), "", name, 0, false, intent.record)
	if err != nil {
		t.Fatalf("ensure scale set %q: %v", name, err)
	}
	if intent.calls != 1 {
		t.Fatalf("the intention was recorded %d times while creating %q; a create that is not"+
			" written down first cannot be recovered from", intent.calls, name)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := gh.DeleteScaleSet(ctx, set.ID); err != nil {
			t.Errorf("cleanup: delete scale set %d: %v", set.ID, err)
		}
	})
	return set
}

// createScaleSet creates a scale set in the group and guarantees its
// deletion, even when the test fails midway.
func createScaleSet(t *testing.T, c *scaleset.Client, group *scaleset.RunnerGroup, name string) *scaleset.RunnerScaleSet {
	t.Helper()
	return createScaleSetWith(t, c, &scaleset.RunnerScaleSet{
		Name:          name,
		RunnerGroupID: group.ID,
	})
}

// createScaleSetWith creates the scale set as described and registers the
// same deletion. It takes the whole request because the label tests vary
// a field createScaleSet does not name, and a second copy of the cleanup
// is a second place for a live fixture to be left behind.
func createScaleSetWith(t *testing.T, c *scaleset.Client, spec *scaleset.RunnerScaleSet) *scaleset.RunnerScaleSet {
	t.Helper()
	name := spec.Name
	created, err := c.CreateRunnerScaleSet(testCtx(t), spec)
	if err != nil {
		t.Fatalf("create scale set %q: %v", name, err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := c.DeleteRunnerScaleSet(ctx, created.ID); err != nil {
			t.Errorf("cleanup: delete scale set %d: %v", created.ID, err)
		}
	})
	if created.ID == 0 {
		t.Fatal("created scale set has no id")
	}
	return created
}

// intentRecorder stands in for the controller.s durable record of the
// name it is about to create. Which branch calls it is a property only
// these contracts can observe: the real provider decides whether the name
// is free, and recording the intention on a path that adopts rather than
// one that creates is what turns a refusal into an adoption on the pass
// that follows it.
type intentRecorder struct{ calls int }

func (r *intentRecorder) record() error {
	r.calls++
	return nil
}

// adoption returns a recorder that fails the test if it is ever called.
// Adoption is not a create, and an intention recorded on this path is the
// record that tells a refusal from a crash — written by a pass that had
// nothing to write down.
func adoption(t *testing.T) *intentRecorder {
	t.Helper()
	r := &intentRecorder{}
	t.Cleanup(func() {
		if r.calls != 0 {
			t.Errorf("adopting an existing scale set recorded the intention to create one, %d times", r.calls)
		}
	})
	return r
}
