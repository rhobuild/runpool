// Package capsulecontract exercises the outer capsule against a real
// daemon and the first-party image: the supervisor protocol, the
// prepare/start separation, and the observation machine that recovery
// depends on. A fake credential drives the whole lifecycle — the real
// runner parses it, rejects it and exits, which proves the runner
// actually ran without needing a provider.
//
// Gated by RUNPOOL_CAPSULE_CONTRACT; the capsule image must exist on
// the daemon (scripts/contracts/capsule.sh builds it first).
package capsulecontract

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rhobuild/runpool/internal/assignment"
	"github.com/rhobuild/runpool/internal/capsule"
	"github.com/rhobuild/runpool/internal/config"
	"github.com/rhobuild/runpool/internal/platform/docker"
)

const (
	image = "runpool-capsule:dev"
	// The outer object names the files the upstream runner materializes; each
	// value is itself base64. The contents are intentionally invalid settings,
	// so the real runner exercises configuration and exits without a provider.
	fakeJITConfig = "eyIucnVubmVyIjoiZTMwPSIsIi5jcmVkZW50aWFscyI6ImUzMD0ifQ=="
)

func launcher(t *testing.T) (*capsule.Launcher, *docker.Client, string) {
	t.Helper()
	if os.Getenv("RUNPOOL_CAPSULE_CONTRACT") == "" {
		t.Skip("set RUNPOOL_CAPSULE_CONTRACT=1 to run against a real daemon with the capsule image")
	}
	dock, err := docker.New(t.Context())
	if err != nil {
		t.Fatalf("connect to daemon: %v", err)
	}
	t.Cleanup(func() { dock.Close() })
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		t.Fatal(err)
	}
	// The gateway runs the same image here: the suite builds one capsule
	// and the launcher is what decides the gateway's, so a test that
	// passed a different one would be testing a shape production cannot
	// produce.
	return capsule.NewLauncher(dock, image), dock, "capcontract-" + hex.EncodeToString(buf)
}

// cgroupDriver reads the driver from the daemon under test. The parent
// cgroup's form depends on it and the daemon rejects the wrong one, so
// no suite may assume which host it is running against.
func cgroupDriver(ctx context.Context, t *testing.T, dock *docker.Client) string {
	t.Helper()
	info, err := dock.Info(ctx)
	if err != nil {
		t.Fatalf("read daemon facts: %v", err)
	}
	return info.CgroupDriver
}

// memRecorder is an in-memory ResourceRecorder: the intent lifecycle
// itself is qualified by the state suite; here it only has to hand the
// cleanup its objects.
type memRecorder struct {
	mu      sync.Mutex
	objects []struct{ kind, id string }
}

func (r *memRecorder) Plan(kind, role, name string) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.objects = append(r.objects, struct{ kind, id string }{kind, name})
	return int64(len(r.objects)), nil
}
func (r *memRecorder) Creating(int64) error { return nil }
func (r *memRecorder) Confirm(id int64, dockerID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.objects[id-1].id = dockerID
	return nil
}

func (r *memRecorder) cleanup(t *testing.T, dock *docker.Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	r.mu.Lock()
	defer r.mu.Unlock()
	// Containers before the network and volumes that hold them.
	for _, o := range r.objects {
		if o.kind == capsule.KindContainer {
			if err := dock.RemoveContainer(ctx, o.id); err != nil {
				t.Errorf("cleanup container %s: %v", o.id, err)
			}
		}
	}
	for _, o := range r.objects {
		switch o.kind {
		case capsule.KindNetwork:
			if err := dock.RemoveNetwork(ctx, o.id); err != nil {
				t.Errorf("cleanup network %s: %v", o.id, err)
			}
		case capsule.KindVolume:
			if err := dock.RemoveVolume(ctx, o.id); err != nil {
				t.Errorf("cleanup volume %s: %v", o.id, err)
			}
		}
	}
}

// cleanupSandbox removes capsule-owned resources before the shared uplink.
// The gateway is attached to both networks, so reversing this order would
// leave the uplink behind and turn a passing contract into a leaking one.
func cleanupSandbox(t *testing.T, dock *docker.Client, rec *memRecorder, uplinkID string) {
	t.Helper()
	rec.cleanup(t, dock)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := dock.RemoveNetwork(ctx, uplinkID); err != nil {
		t.Errorf("cleanup uplink network %s: %v", uplinkID, err)
	}
}

// TestCapsuleLifecycle drives the whole machine: prepare proves
// readiness with the runner deliberately unstarted, inspection reports
// exactly that, the authorized start launches the real runner, and the
// serving ends the way the platform ends one - a TERM to the supervisor,
// whose drain is what docker stop and a host shutdown deliver.
//
// The runner cannot be made to exit by the credential itself: a listener
// that cannot use its configuration exits retryable, and the upstream
// run helper relaunches it without bound. Running is therefore the
// steady state a fake credential produces, which is exactly what the
// running observation needs, and the drain is what ends it.
func TestCapsuleLifecycle(t *testing.T) {
	m, dock, leaseID := launcher(t)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()

	rec := &memRecorder{}
	t.Cleanup(func() { rec.cleanup(t, dock) })

	prepared, err := m.Prepare(ctx, capsule.Spec{
		LeaseID:      leaseID,
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

	// Prepared means ready-and-waiting: the daemon booted, and no start
	// has taken effect — which inspection must prove, because recovery
	// requeues on exactly this answer.
	obs, err := m.InspectExecution(ctx, prepared)
	if err != nil || obs != assignment.ObservedCreated {
		t.Fatalf("pre-start observation = %s, %v; want created", obs, err)
	}

	if err := m.Start(ctx, prepared); err != nil {
		t.Fatalf("start: %v", err)
	}

	// The authorized start is observable as running while the runner
	// holds the fake credential's retry loop.
	deadline := time.Now().Add(2 * time.Minute)
	var last assignment.ExecutionObservation
	for time.Now().Before(deadline) {
		last, err = m.InspectExecution(ctx, prepared)
		if err != nil && last == assignment.ObservedUnavailable {
			t.Logf("transient inspection: %v", err)
		}
		if last == assignment.ObservedRunning {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if last != assignment.ObservedRunning {
		t.Fatalf("post-start observation never reached running; last = %s", last)
	}

	// Running says the runner was forked, not that it has read anything:
	// the supervisor writes that state immediately after fork/exec,
	// which is the earliest moment at which the job can be said to be
	// the runner's. The drain below has to reach a listener that is
	// actually up, so wait for the listener's own first output rather
	// than for the state that precedes it.
	deadline = time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		tail, err := dock.TailLogs(ctx, prepared.RuntimeID, 200)
		if err != nil {
			t.Fatalf("logs: %v", err)
		}
		if strings.Contains(tail, "[RUNNER") {
			break
		}
		time.Sleep(2 * time.Second)
	}

	// End the serving the way the platform ends one. PID 1 is the
	// supervisor, and its TERM path drains the runner and the inner
	// daemon before reporting the exit.
	if code, out, err := dock.ExecWithInput(ctx, prepared.RuntimeID,
		[]string{"kill", "-TERM", "1"}, nil); err != nil || code != 0 {
		t.Fatalf("signal the supervisor: exit %d, %v: %s", code, err, out)
	}

	deadline = time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		last, err = m.InspectExecution(ctx, prepared)
		if err != nil && last == assignment.ObservedUnavailable {
			t.Logf("transient inspection: %v", err)
		}
		if last == assignment.ObservedExited {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if last != assignment.ObservedExited {
		t.Fatalf("drained observation never reached exited; last = %s", last)
	}

	// The capsule's exit is the job's exit: the daemon-side wait must
	// agree with the observation.
	exit, err := dock.WaitExit(ctx, prepared.RuntimeID)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	t.Logf("capsule exited %d after the drain ended the runner", exit)

	// The JIT bundle must not have reached the container log driver.
	tail, err := dock.TailLogs(ctx, prepared.RuntimeID, 200)
	if err != nil {
		t.Fatalf("logs: %v", err)
	}
	if strings.Contains(tail, fakeJITConfig) {
		t.Error("the credential appears in the capsule log; the delivery channel leaked it")
	}

	// The listener has to have run and to have failed in its own
	// controlled way. A crash aborts before the credential is parsed and
	// proves nothing about delivery - and the upstream helper masks it
	// into the same exit a healthy runner produces, so only the log
	// tells.
	if !strings.Contains(tail, "[RUNNER") {
		t.Error("no runner output in the capsule log; the listener never ran")
	}
	for _, marker := range []string{"Unhandled exception", "core dumped"} {
		if strings.Contains(tail, marker) {
			t.Errorf("the runner crashed rather than running (%q in the log); a crash "+
				"before the credential is parsed proves nothing about delivery", marker)
		}
	}
}
