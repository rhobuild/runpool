package capsulecontract

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rhobuild/runpool/internal/assignment"
	"github.com/rhobuild/runpool/internal/capsule"
	"github.com/rhobuild/runpool/internal/capsule/protocol"
	"github.com/rhobuild/runpool/internal/config"
	"github.com/rhobuild/runpool/internal/platform/docker"
)

// controlProtocolFile is where the supervisor declares what it speaks.
// The path is written out rather than borrowed from the launcher because
// it is the contract itself: the two sides meet at this location, and a
// test that read the launcher's own constant would agree with it however
// wrong it was.
const controlProtocolFile = "/run/runpool/protocol"

// innerDockerSocket is the daemon the job actually runs on. The launcher
// never names it — the supervisor owns it — so a test that asks whether
// the daemon is up has to address it directly.
const innerDockerSocket = "unix:///run/runpool-docker/docker.sock"

// prepared brings up one capsule and hands back what the tests here need
// from it. Every one of them is about the moment prepare returns, so
// none of them starts the runner.
func prepared(t *testing.T) (*capsule.Launcher, *docker.Client, capsule.PreparedRuntime) {
	t.Helper()
	m, dock, leaseID := launcher(t)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()

	rec := &memRecorder{}
	t.Cleanup(func() { rec.cleanup(t, dock) })

	runtime, err := m.Prepare(ctx, capsule.Spec{
		LeaseID:      assignment.LeaseID(leaseID),
		InstanceID:   "contract",
		CapsuleImage: image,
		JITConfig:    fakeJITConfig,
		Resources: config.Resources{
			Memory: 2 << 30, Swap: 0, CPU: 2e9, PIDs: 512,
		},
		CgroupDriver: cgroupDriver(ctx, t, dock),
	}, rec)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	return m, dock, runtime
}

// TestTheCapsuleDeclaresTheProtocolThisBuildSpeaks pins the constant
// against the image rather than against itself.
//
// The launcher refuses a capsule whose declaration is not this build's,
// so a bumped constant and a stale image already fail — but they fail as
// "incompatible image", which is also what a genuinely foreign image
// produces. This says which of the two happened, and it is the only
// check that can: the declaration is baked into the image at build time,
// and nothing hermetic can read it.
func TestTheCapsuleDeclaresTheProtocolThisBuildSpeaks(t *testing.T) {
	_, dock, runtime := prepared(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	code, out, err := dock.Exec(ctx, string(runtime.RuntimeID), []string{"cat", controlProtocolFile})
	if err != nil || code != 0 {
		t.Fatalf("read %s: exit %d, %v: %s", controlProtocolFile, code, err, out)
	}
	if got := strings.TrimSpace(out); got != protocol.Version {
		t.Errorf("the capsule image declares protocol %q and this build speaks %q; "+
			"the image was not rebuilt for the change that moved the version", got, protocol.Version)
	}
}

// TestPrepareWaitsForAProvenDaemon: when prepare returns, the capsule
// can run a job.
//
// The supervisor writes `waiting` and the launcher waits for it, so what
// this proves is that the name means what the launcher reads it as. A
// supervisor that writes it at boot satisfies the launcher just as well
// and has no daemon behind it — and the credential is delivered and the
// start authorized against that capsule anyway, which is the failure
// this pins. Only the pair can demonstrate it: the state is written on
// one side and waited on from the other.
func TestPrepareWaitsForAProvenDaemon(t *testing.T) {
	_, dock, runtime := prepared(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	// Asked once, with no retry on purpose. A poll here would prove only
	// that the daemon comes up eventually, which was never in doubt; the
	// claim is that it is already up at the instant prepare returned.
	code, out, err := dock.Exec(ctx, string(runtime.RuntimeID), []string{"docker", "--host=" + innerDockerSocket, "info"})
	if err != nil {
		t.Fatalf("probe the inner daemon: %v", err)
	}
	if code != 0 {
		t.Errorf("prepare returned while the inner daemon was not answering (exit %d): %s\n"+
			"a job authorized against this capsule has nothing to run on", code, out)
	}
}

// TestAbortBeforeStartExitsWithTheReservedCode: a capsule stopped before
// its start is authorized reports that the runner never owned the job.
//
// This is the ordinary shutdown of an idle capsule — the platform stops
// the controller, the daemon stops the container — and the only account
// it can leave is its exit code: the state file is tmpfs and dies with
// it. Reporting a clean exit here settles an attempt that never ran as
// complete, and nothing requeues it afterwards.
func TestAbortBeforeStartExitsWithTheReservedCode(t *testing.T) {
	m, dock, runtime := prepared(t)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()

	// Nothing was started, and the capsule says so while it can still be
	// asked. The same answer has to survive the stop.
	if obs, err := m.InspectExecution(ctx, runtime); err != nil || obs != assignment.ObservedCreated {
		t.Fatalf("pre-stop observation = %s, %v; want created", obs, err)
	}

	if code, out, err := dock.ExecWithInput(ctx, string(runtime.RuntimeID), []string{"kill", "-TERM", "1"}, nil); err != nil || code != 0 {
		t.Fatalf("signal the supervisor: exit %d, %v: %s", code, err, out)
	}

	exit, err := dock.WaitExit(ctx, string(runtime.RuntimeID))
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if int(exit) != protocol.AbortedExitCode {
		t.Errorf("a capsule stopped before its start exited %d; want the reserved %d, "+
			"which is what tells the controller the job was never handed over",
			exit, protocol.AbortedExitCode)
	}

	// And the controller's own reading of that code, through the seam
	// recovery uses: any other answer settles the attempt as a run.
	obs, err := m.InspectExecution(ctx, runtime)
	if err != nil {
		t.Fatalf("post-stop inspection: %v", err)
	}
	if obs != assignment.ObservedCreated {
		t.Errorf("post-stop observation = %s; want created, so the workload returns to the queue", obs)
	}
}
