package githubcontract

import (
	"testing"
)

// TestRepositoryScaleSetAndJitRunner covers the repository scope — the
// only scope allowed to bind a persistent cache — through the
// production wrapper, including the JIT provisioning flow: generating a
// JIT config registers a runner that never started, which must be
// findable by name and removable — exactly what failure cleanup relies
// on. The not-found translation (found=false, never a nil to trip
// over) is proven live on the way.
func TestRepositoryScaleSetAndJitRunner(t *testing.T) {
	url, token := target(t, envRepoURL, envRepoToken)
	gh := newWrapper(t, url, token)

	name := uniqueName(t)
	created := ensureSet(t, gh, name)
	t.Logf("created repository scale set: id=%d group=%d", created.ID, created.GroupID)

	runnerName := name + "-r1"
	jit, err := gh.GenerateJITConfig(testCtx(t), created.ID, runnerName, "_work")
	if err != nil {
		t.Fatalf("generate jit config: %v", err)
	}
	if jit.RunnerName != runnerName || jit.RunnerID == 0 || jit.Encoded == "" {
		t.Fatalf("jit config incomplete: id=%d name=%q encoded=%d bytes", jit.RunnerID, jit.RunnerName, len(jit.Encoded))
	}
	t.Logf("jit runner registered: id=%d name=%q (never started)", jit.RunnerID, jit.RunnerName)

	ref, found, err := gh.RunnerByName(testCtx(t), runnerName)
	if err != nil || !found || ref.ID != jit.RunnerID {
		t.Fatalf("runner by name = %+v, found=%v, %v; want id %d", ref, found, err, jit.RunnerID)
	}

	if err := gh.RemoveRunner(testCtx(t), jit.RunnerID); err != nil {
		t.Fatalf("remove never-started jit runner: %v", err)
	}
	if _, found, err := gh.RunnerByName(testCtx(t), runnerName); err != nil || found {
		t.Fatalf("after removal: found=%v, %v; want a clean not-found", found, err)
	}
}
