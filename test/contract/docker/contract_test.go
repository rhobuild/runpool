// Package dockercontract is the live contract suite for the pinned
// github.com/moby/moby/client — the daemon mechanics every capsule rests
// on. Compilation cannot verify that options were mapped to the correct API
// fields, so the boundary is exercised against a real daemon.
//
// The suite is gated: without RUNPOOL_DOCKER_CONTRACT set it skips, so
// `go test ./...` stays hermetic. Every resource carries the ownership
// labels under a unique run-scoped instance id and is removed by
// cleanup, so an aborted run leaves objects that ListOwned* can find.
//
//	RUNPOOL_DOCKER_CONTRACT=1 go test ./test/contract/docker/...
package dockercontract

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rhobuild/runpool/internal/cache"
	"github.com/rhobuild/runpool/internal/engine"
	"github.com/rhobuild/runpool/internal/engine/docker"
	"github.com/rhobuild/runpool/internal/platform"
	"github.com/rhobuild/runpool/internal/store"

	"github.com/rhobuild/runpool/internal/assignment"
)

// busybox is the smallest image that can run a command. It is pinned by
// digest like every image the product itself creates containers from: a
// moving tag would make a failure here unreproducible, and "it is only
// test scaffolding" is how an unpinned dependency gets in.
const busybox = "busybox:1.37@sha256:f85340bf132ae937d2c2a763b8335c9bab35d6e8293f70f606b9c6178d84f42b"

// missingImage exercises the pull-on-missing path: the harness removes
// it from the daemon before the suite runs, and verifies the removal,
// so the pull is real on every run. It is digest-pinned like everything
// else — what makes the path real is the daemon-side removal, not a
// moving reference.
const missingImage = "busybox:1.36.1-uclibc@sha256:0872fb3a7632ba9d0ae46a8e832a62b30ce83a6f220b8bb52903d9cf477dabe3"

func client(t *testing.T) (*docker.Client, string) {
	t.Helper()
	if os.Getenv("RUNPOOL_DOCKER_CONTRACT") == "" {
		t.Skip("set RUNPOOL_DOCKER_CONTRACT=1 to run against a real daemon")
	}
	c, err := docker.New(t.Context())
	if err != nil {
		t.Fatalf("connect to daemon: %v", err)
	}
	t.Cleanup(func() { c.Close() })

	// A unique instance id per test keeps the ownership queries scoped
	// to what this test created, even with suites running in parallel.
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("generate a run-scoped instance id: %v", err)
	}
	return c, "contract-" + hex.EncodeToString(buf)
}

func labels(instance, lease, kind, role string) map[string]string {
	return map[string]string{
		"io.runpool.managed":  "true",
		"io.runpool.instance": instance,
		"io.runpool.lease":    lease,
		"io.runpool.kind":     kind,
		"io.runpool.role":     role,
	}
}

// TestContainerLifecycle covers create, start, wait and log capture —
// the RunTask path every chown and seed helper takes.
func TestContainerLifecycle(t *testing.T) {
	c, instance := client(t)
	ctx := t.Context()

	code, out, err := c.RunTask(ctx, engine.ContainerSpec{
		Name:   instance + "-task",
		Image:  busybox,
		Cmd:    []string{"sh", "-c", "echo marker-out; echo marker-err >&2; exit 7"},
		Labels: labels(instance, "lease-task", "task", "helper"),
	})
	if err != nil {
		t.Fatalf("run task: %v", err)
	}
	if code != 7 {
		t.Errorf("exit code = %d; want 7 — the wait result is read from the wrong channel", code)
	}
	// Both streams demultiplexed: stdcopy moved packages in the
	// migration, and a wrong reader yields framing bytes or nothing.
	if !strings.Contains(out, "marker-out") || !strings.Contains(out, "marker-err") {
		t.Errorf("combined output = %q; want both stream markers", out)
	}
}

// TestRemoveIsIdempotent is the contract the cleanup saga depends on:
// removing what is already gone must succeed. It is decided by the
// client's typed errors, and the migration changed which module those
// errors come from.
func TestRemoveIsIdempotent(t *testing.T) {
	c, instance := client(t)
	ctx := t.Context()

	id, err := c.CreateContainer(ctx, engine.ContainerSpec{
		Name:   instance + "-gone",
		Image:  busybox,
		Cmd:    []string{"true"},
		Labels: labels(instance, "lease-gone", "task", "helper"),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Registered here rather than left to the removals below: this suite
	// runs against a daemon it does not own, and any t.Fatalf between
	// the create and the last remove leaves a container behind that
	// nothing else will ever collect. It cannot mask what is being
	// tested — that removing what is already gone is success is the
	// assertion two lines down.
	t.Cleanup(func() { _ = c.RemoveContainer(context.Background(), id) })
	if err := c.RemoveContainer(ctx, id); err != nil {
		t.Fatalf("first remove: %v", err)
	}
	if err := c.RemoveContainer(ctx, id); err != nil {
		t.Errorf("second remove: %v; absence must be success or a crashed cleanup can never finish", err)
	}
	if err := c.RemoveVolume(ctx, instance+"-never-created"); err != nil {
		t.Errorf("remove absent volume: %v", err)
	}
	if err := c.RemoveNetwork(ctx, instance+"-never-created"); err != nil {
		t.Errorf("remove absent network: %v", err)
	}
}

// TestPullOnMissingImage covers the one create failure worth retrying.
// The retry is chosen by a typed not-found check, so it fails closed if
// the client stops classifying that error.
func TestPullOnMissingImage(t *testing.T) {
	c, instance := client(t)
	ctx := t.Context()

	// A tag no local daemon has cached: create must fail not-found,
	// pull, and succeed on the second attempt.
	id, err := c.CreateContainer(ctx, engine.ContainerSpec{
		Name:   instance + "-pull",
		Image:  missingImage,
		Cmd:    []string{"true"},
		Labels: labels(instance, "lease-pull", "task", "helper"),
	})
	if err != nil {
		t.Fatalf("create with a missing image: %v", err)
	}
	t.Cleanup(func() { mustRemoveContainer(t, c, ctx, id) })
}

// TestExec covers the readiness-probe path: the capsule reads the dind
// socket gid and polls daemon liveness this way. Exec was renamed in the
// migration (ContainerExec* became Exec*), so this is the call most
// likely to have been mapped to the wrong method.
func TestExec(t *testing.T) {
	c, instance := client(t)
	ctx := t.Context()

	id, err := c.CreateContainer(ctx, engine.ContainerSpec{
		Name:   instance + "-exec",
		Image:  busybox,
		Cmd:    []string{"sleep", "60"},
		Labels: labels(instance, "lease-exec", "task", "helper"),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { mustRemoveContainer(t, c, ctx, id) })
	if err := c.StartContainer(ctx, id); err != nil {
		t.Fatalf("start: %v", err)
	}

	code, out, err := c.Exec(ctx, id, []string{"sh", "-c", "echo from-exec; exit 3"})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if code != 3 {
		t.Errorf("exec exit = %d; want 3 — the code is read from the inspect result", code)
	}
	if !strings.Contains(out, "from-exec") {
		t.Errorf("exec output = %q; want the command's stdout", out)
	}
	// Zero and non-zero both come back as codes rather than as errors: a
	// command that ran and failed is an answer, and only a command that
	// could not be run is an error.
	for command, want := range map[string]int{"true": 0, "false": 1} {
		code, _, err := c.Exec(ctx, id, []string{command})
		if err != nil {
			t.Errorf("exec %q: %v", command, err)
			continue
		}
		if code != want {
			t.Errorf("exec %q exited %d; want %d", command, code, want)
		}
	}
}

// TestExecHonoursItsContext: an exec ends when its context ends.
//
// Nothing in the Docker API cancels a running exec. The command keeps
// running inside the container, and the client's only lever is the
// hijacked connection it is reading from — so the deadline is honoured
// by closing that connection, not by the daemon. Compilation cannot say
// whether that wiring survived, and the callers that depend on it are
// the ones nothing else proceeds past: the launch goroutine the drain
// counts, and the reconciliation pass every later pass queues behind. An
// exec that outlives its context there is an unbounded shutdown.
func TestExecHonoursItsContext(t *testing.T) {
	c, instance := client(t)
	ctx := t.Context()

	id, err := c.CreateContainer(ctx, engine.ContainerSpec{
		Name:   instance + "-exec-ctx",
		Image:  busybox,
		Cmd:    []string{"sleep", "300"},
		Labels: labels(instance, "lease-exec-ctx", "task", "helper"),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { mustRemoveContainer(t, c, ctx, id) })
	if err := c.StartContainer(ctx, id); err != nil {
		t.Fatalf("start: %v", err)
	}

	// The command outlasts the context thirtyfold, so a return inside
	// the bound cannot be the command finishing.
	bounded, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	start := time.Now()
	_, _, err = c.Exec(bounded, id, []string{"sleep", "60"})
	elapsed := time.Since(start)

	if err == nil {
		t.Error("exec returned no error after its context expired; the caller cannot tell it was cut short")
	}
	// Ten seconds is five times the bound and a sixth of the command. A
	// looser window would let a return that owes nothing to the context
	// pass for one that does.
	if elapsed > 10*time.Second {
		t.Fatalf("exec returned after %v with a 2s context; it is bounded by the command, not the caller", elapsed)
	}
}

// TestExecWithInput covers the credential channel: stdin into an exec
// is the one path into a running container that Docker persists nowhere
// — not in the container config, not in an image layer, not in the log
// driver. The digest proves the bytes arrived intact without the test
// creating a second copy of them, and the log check proves the channel
// itself leaks nothing.
func TestExecWithInput(t *testing.T) {
	c, instance := client(t)
	ctx := t.Context()

	id, err := c.CreateContainer(ctx, engine.ContainerSpec{
		Name:   instance + "-stdin",
		Image:  busybox,
		Cmd:    []string{"sleep", "60"},
		Tmpfs:  map[string]string{"/secrets": "rw,size=64k"},
		Labels: labels(instance, "lease-stdin", "task", "helper"),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { mustRemoveContainer(t, c, ctx, id) })
	if err := c.StartContainer(ctx, id); err != nil {
		t.Fatalf("start: %v", err)
	}

	secret := []byte("contract-value")
	want := sha256.Sum256(secret)
	code, out, err := c.ExecWithInput(ctx, id,
		[]string{"sh", "-c", "cat > /secrets/token && sha256sum /secrets/token | cut -d' ' -f1"},
		secret)
	if err != nil || code != 0 {
		t.Fatalf("exec with input: exit %d, %v: %s", code, err, out)
	}
	if !strings.Contains(out, hex.EncodeToString(want[:])) {
		t.Errorf("the payload did not arrive intact on the tmpfs; digest output = %q", out)
	}

	// Exec streams go to the attach connection only; nothing may reach
	// the container's log driver, which outlives the tmpfs.
	logs, err := c.TailLogs(ctx, id, 50)
	if err != nil {
		t.Fatalf("logs: %v", err)
	}
	if strings.Contains(logs, string(secret)) || strings.Contains(logs, hex.EncodeToString(want[:])) {
		t.Error("the exec stream reached the log driver; the channel is not private")
	}
}

// TestOwnershipQueries is the reconciliation working set: after a crash
// the controller finds what it owns through labels alone. All three
// list calls changed result shape in the migration — a wrong field
// yields an empty slice, which reads as "nothing to reconcile".
func TestOwnershipQueries(t *testing.T) {
	c, instance := client(t)
	ctx := t.Context()

	id, err := c.CreateContainer(ctx, engine.ContainerSpec{
		Name:   instance + "-owned",
		Image:  busybox,
		Cmd:    []string{"sleep", "60"},
		Labels: labels(instance, "lease-owned", "capsule", "runner"),
	})
	if err != nil {
		t.Fatalf("create container: %v", err)
	}
	t.Cleanup(func() { mustRemoveContainer(t, c, ctx, id) })
	if err := c.StartContainer(ctx, id); err != nil {
		t.Fatalf("start: %v", err)
	}

	vol := instance + "-owned-vol"
	if _, err := c.CreateVolume(ctx, vol, labels(instance, "lease-owned", "volume", "workspace")); err != nil {
		t.Fatalf("create volume: %v", err)
	}
	t.Cleanup(func() {
		if err := c.RemoveVolume(context.WithoutCancel(ctx), vol); err != nil {
			t.Errorf("cleanup: volume %s was not removed: %v", vol, err)
		}
	})

	netID, err := c.CreateNetwork(ctx, engine.NetworkSpec{
		Name: instance + "-owned-net", Labels: labels(instance, "lease-owned", "network", "capsule"),
	})
	if err != nil {
		t.Fatalf("create network: %v", err)
	}
	t.Cleanup(func() {
		if err := c.RemoveNetwork(context.WithoutCancel(ctx), netID); err != nil {
			t.Errorf("cleanup: network %s was not removed: %v", netID, err)
		}
	})

	containers, err := c.ListOwnedContainers(ctx, assignment.InstanceID(instance))
	if err != nil {
		t.Fatalf("list containers: %v", err)
	}
	if len(containers) != 1 {
		t.Fatalf("owned containers = %d; want 1", len(containers))
	}
	got := containers[0]
	if got.Name != instance+"-owned" || got.Kind != "capsule" || got.Role != "runner" || got.LeaseID != "lease-owned" {
		t.Errorf("owned container = %+v; labels did not survive the round trip", got)
	}
	// Running state decides whether recovery adopts or cleans up.
	if !got.Running {
		t.Error("a started container reported as not running; recovery would clean up live work")
	}

	volumes, err := c.ListOwnedVolumes(ctx, assignment.InstanceID(instance))
	if err != nil {
		t.Fatalf("list volumes: %v", err)
	}
	if len(volumes) != 1 || volumes[0].ID != vol || volumes[0].LeaseID != "lease-owned" {
		t.Errorf("owned volumes = %+v; want the one just created", volumes)
	}

	networks, err := c.ListOwnedNetworks(ctx, assignment.InstanceID(instance))
	if err != nil {
		t.Fatalf("list networks: %v", err)
	}
	if len(networks) != 1 || networks[0].ID != netID || networks[0].LeaseID != "lease-owned" {
		t.Errorf("owned networks = %+v; want the one just created", networks)
	}

	// Another instance's query must not see any of it: the instance
	// label is what keeps two controllers on one host from sweeping
	// each other's capsules.
	foreign, err := c.ListOwnedContainers(ctx, assignment.InstanceID(instance+"-other"))
	if err != nil {
		t.Fatalf("list for a foreign instance: %v", err)
	}
	if len(foreign) != 0 {
		t.Errorf("a foreign instance sees %d containers; ownership is not scoped", len(foreign))
	}
}

// TestResourceLimits confirms the tier envelope reaches the daemon.
// The limits are set through a nested Resources struct that the
// migration moved inside a new options envelope; silently dropping them
// would leave every capsule unbounded.
func TestResourceLimits(t *testing.T) {
	c, instance := client(t)
	ctx := t.Context()

	const (
		memBytes  = 64 * 1024 * 1024
		swapBytes = 32 * 1024 * 1024
	)
	code, out, err := c.RunTask(ctx, engine.ContainerSpec{
		Name:  instance + "-limits",
		Image: busybox,
		// cgroup v2 reports the container's own envelope in these files.
		// Checking only memory.max would let swap, CPU or PIDs be
		// dropped silently — and an unbounded fork or CPU budget is as
		// much a capacity lie as unbounded memory.
		Cmd: []string{"sh", "-c",
			"cat /sys/fs/cgroup/memory.max /sys/fs/cgroup/memory.swap.max " +
				"/sys/fs/cgroup/cpu.max /sys/fs/cgroup/pids.max"},
		Labels:          labels(instance, "lease-limits", "task", "helper"),
		MemoryBytes:     memBytes,
		MemorySwapBytes: memBytes + swapBytes,
		NanoCPUs:        500_000_000,
		PIDsLimit:       128,
	})
	if err != nil {
		t.Fatalf("run task: %v", err)
	}
	if code != 0 {
		t.Fatalf("reading the cgroup limit exited %d: %s", code, out)
	}
	// The four files are read in a fixed order, so the output is
	// position-for-position comparable. Substring matching here once let
	// a value pass by appearing anywhere — "0" matches inside
	// "67108864" — which is no check at all.
	lines := strings.Fields(strings.TrimSpace(out))
	// cpu.max is one line with two fields (quota period), so five fields
	// in total.
	want := []string{
		"67108864", // memory.max
		"33554432", // memory.swap.max — additional swap, not Docker's combined value
		"50000",    // cpu.max quota for 0.5 CPU
		"100000",   // cpu.max period, the daemon default
		"128",      // pids.max
	}
	if len(lines) != len(want) {
		t.Fatalf("cgroup readback has %d fields, want %d:\n%s", len(lines), len(want), out)
	}
	names := []string{"memory.max", "memory.swap.max", "cpu.max quota", "cpu.max period", "pids.max"}
	for i, w := range want {
		if lines[i] != w {
			t.Errorf("%s = %q; want %q — the tier envelope did not reach the daemon intact",
				names[i], lines[i], w)
		}
	}
}

// TestOwnedIDByName is the fail-closed resolution the intent model
// rests on: an object found by its deterministic name is returned only
// with proven ownership, a foreign object with the same name is a typed
// refusal, and absence is an answer, not an error.
func TestOwnedIDByName(t *testing.T) {
	c, instance := client(t)
	ctx := t.Context()

	name := instance + "-owned-by-name"
	id, err := c.CreateContainer(ctx, engine.ContainerSpec{
		Name:   name,
		Image:  busybox,
		Cmd:    []string{"true"},
		Labels: labels(instance, "lease-own", "container", "runner"),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { mustRemoveContainer(t, c, ctx, id) })

	got, err := c.OwnedIDByName(ctx, "container", name, assignment.InstanceID(instance), "lease-own")
	if err != nil || got != id {
		t.Errorf("owned resolution = %q, %v; want the created id", got, err)
	}

	// Same name, different lease: name equality is not ownership.
	if _, err := c.OwnedIDByName(ctx, "container", name, assignment.InstanceID(instance), "someone-else"); !errors.Is(err, engine.ErrForeignResource) {
		t.Errorf("foreign resolution = %v; want ErrForeignResource — adopting by name "+
			"would run work through someone else's object and later delete it", err)
	}
	if err := c.RemoveOwnedContainer(ctx, name, assignment.InstanceID(instance), "someone-else"); !errors.Is(err, engine.ErrForeignResource) {
		t.Fatalf("foreign removal = %v; want ErrForeignResource", err)
	}
	if got, err := c.OwnedIDByName(ctx, "container", name, assignment.InstanceID(instance), "lease-own"); err != nil || got != id {
		t.Fatalf("foreign removal changed the owned object: id=%q err=%v", got, err)
	}

	// Absence is an answer: the create never took effect.
	if got, err := c.OwnedIDByName(ctx, "container", instance+"-never-existed", assignment.InstanceID(instance), "lease-own"); err != nil || got != "" {
		t.Errorf("absent resolution = %q, %v; want empty and no error", got, err)
	}
}

// TestOwnedRemovalRefusesForeignNetworkAndVolume covers the destructive
// branches whose daemon identity is not a container id. In particular, Docker
// volumes are addressed only by name, so the immediate label proof is the
// guard against deleting a name another owner now holds.
func TestOwnedRemovalRefusesForeignNetworkAndVolume(t *testing.T) {
	c, instance := client(t)
	ctx := t.Context()

	networkName := instance + "-owned-removal-network"
	networkID, err := c.CreateNetwork(ctx, engine.NetworkSpec{
		Name:   networkName,
		Labels: labels(instance, "lease-owner", "network", "capsule-net"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.RemoveNetwork(context.WithoutCancel(ctx), networkID); err != nil {
			t.Errorf("cleanup network %s: %v", networkID, err)
		}
	})
	if err := c.RemoveOwnedNetwork(ctx, networkID, assignment.InstanceID(instance), "different-lease"); !errors.Is(err, engine.ErrForeignResource) {
		t.Fatalf("foreign network removal = %v; want ErrForeignResource", err)
	}
	if got, err := c.OwnedIDByName(ctx, "network", networkName, assignment.InstanceID(instance), "lease-owner"); err != nil || got != networkID {
		t.Fatalf("network changed after refused removal: id=%q err=%v", got, err)
	}

	volumeName := instance + "-owned-removal-volume"
	if _, err := c.CreateVolume(ctx, volumeName, labels(instance, "lease-owner", "volume", "dind-data")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.RemoveVolume(context.WithoutCancel(ctx), volumeName); err != nil {
			t.Errorf("cleanup volume %s: %v", volumeName, err)
		}
	})
	if err := c.RemoveOwnedVolume(ctx, volumeName, assignment.InstanceID(instance), "different-lease"); !errors.Is(err, engine.ErrForeignResource) {
		t.Fatalf("foreign volume removal = %v; want ErrForeignResource", err)
	}
	if got, err := c.OwnedIDByName(ctx, "volume", volumeName, assignment.InstanceID(instance), "lease-owner"); err != nil || got != volumeName {
		t.Fatalf("volume changed after refused removal: id=%q err=%v", got, err)
	}
}

// lockArch translates the daemon's spelling of an architecture into the
// one the platform lock records. They differ on both platforms a release builds
// for, and scripts/qualification/platform-facts.sh maps the same pair
// for the facts the release gate reads.
func lockArch(daemon string) string {
	switch daemon {
	case "x86_64":
		return "amd64"
	case "aarch64":
		return "arm64"
	}
	return daemon
}

// TestTheDaemonSpellingSelectsTheEntry: the entry is selected by the
// architecture the daemon reports, and the daemon does not spell it the
// way the lock does.
//
// Mapping one architecture and not the other refuses a host that ran the
// suites for how its daemon writes the name, and it refuses it as a
// platform nobody qualified -- which is the answer a record of several
// platforms exists to stop giving.
func TestTheDaemonSpellingSelectsTheEntry(t *testing.T) {
	for daemon, want := range map[string]string{"x86_64": "amd64", "aarch64": "arm64"} {
		if got := lockArch(daemon); got != want {
			t.Errorf("a daemon reporting %q selects the %q entry; the lock records %q, so the "+
				"host is told nobody has qualified its platform", daemon, got, want)
		}
	}
}

// TestHostInfo covers the doctor's view of the daemon. Runpool refuses
// hosts it cannot honour the capsule contract on, and every field here
// is read from a result struct the migration introduced.
//
// Under RUNPOOL_CONTRACT_QUALIFY the check hardens from populated fields to
// an exact match with the release-qualification reference.
func TestHostInfo(t *testing.T) {
	c, _ := client(t)

	info, err := c.Info(t.Context())
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	if info.ServerVersion == "" || info.APIVersion == "" {
		t.Errorf("info = %+v; version fields are how the host gate decides", info)
	}
	if info.CgroupVersion == "" {
		t.Error("cgroup version is empty; the v2 requirement cannot be checked")
	}
	if info.OSType == "" || info.Architecture == "" {
		t.Errorf("info = %+v; platform fields are empty", info)
	}

	if os.Getenv("RUNPOOL_CONTRACT_QUALIFY") == "" {
		return
	}
	// The expectation comes from the reviewed manifest, never from the host
	// being evaluated.
	reference := platform.MustLoad()
	arch := lockArch(info.Architecture)
	// The entry for the host being measured. A host with none is not a
	// host that failed the reference; it is one nobody has qualified, and
	// saying so is what keeps this from reading as a broken release.
	qualified, ok := reference.For(arch)
	if !ok {
		t.Fatalf("not the release-qualification reference — %v", reference.NotQualified(arch))
	}
	rootless := info.Rootless
	observed := platform.Facts{
		Engine:        info.ServerVersion,
		API:           info.APIVersion,
		Arch:          arch,
		CgroupVersion: info.CgroupVersion,
		CgroupDriver:  info.CgroupDriver,
		Rootless:      &rootless,
	}
	for _, m := range qualified.CompareDockerFacts(observed) {
		t.Errorf("not the release-qualification reference — %s", m)
	}
	// OSType is compared here rather than through the manifest, which
	// records no operating-system kind. The architecture is not: it is
	// what selected the entry above, so comparing it again would compare
	// a value against itself.
	if info.OSType != "linux" {
		t.Errorf("platform = %s/%s; every qualification reference is a Linux host",
			info.OSType, info.Architecture)
	}
	t.Logf("release-qualification reference confirmed: engine %s, api %s, cgroup v%s/%s, rootful",
		info.ServerVersion, info.APIVersion, info.CgroupVersion, info.CgroupDriver)
}

// TestEnsureOwnedVolume is the fail-closed half of the cache hand-off:
// Docker's volume create silently adopts an existing name whatever its
// labels say, so the ensure must prove ownership itself. A foreign
// volume with the expected name must never be handed to a job.
func TestEnsureOwnedVolume(t *testing.T) {
	c, instance := client(t)
	ctx := t.Context()

	own := map[string]string{
		"io.runpool.managed":  "true",
		"io.runpool.instance": instance,
		"io.runpool.role":     cache.RoleCacheLane,
	}
	name := instance + "-ensure"
	t.Cleanup(func() { _ = c.RemoveVolume(context.Background(), name) })

	if err := c.EnsureOwnedVolume(ctx, name, own); err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	// Idempotent: the second ensure adopts the volume it created.
	if err := c.EnsureOwnedVolume(ctx, name, own); err != nil {
		t.Fatalf("second ensure: %v", err)
	}

	foreign := instance + "-foreign"
	t.Cleanup(func() { _ = c.RemoveVolume(context.Background(), foreign) })
	if _, err := c.CreateVolume(ctx, foreign, map[string]string{"someone": "else"}); err != nil {
		t.Fatal(err)
	}
	err := c.EnsureOwnedVolume(ctx, foreign, own)
	if !errors.Is(err, engine.ErrForeignResource) {
		t.Fatalf("ensuring over a foreign volume = %v; want ErrForeignResource", err)
	}
}

// TestCreateVolumeRefusesATakenName is the other half, and the one the
// capsule depends on. The daemon's volume create is the only create
// behind this port that answers a taken name with the volume that is
// already there and no error, so a launcher built on collisions being
// reported would adopt a stranger's volume and mount it as the data root
// of a privileged daemon. The adapter is what makes the daemon's answer
// match the port's contract.
func TestCreateVolumeRefusesATakenName(t *testing.T) {
	c, instance := client(t)
	ctx := t.Context()

	name := instance + "-taken"
	t.Cleanup(func() { _ = c.RemoveVolume(context.Background(), name) })
	if _, err := c.CreateVolume(ctx, name, map[string]string{"someone": "else"}); err != nil {
		t.Fatalf("seeding the volume that is already there: %v", err)
	}

	own := map[string]string{
		"io.runpool.managed":  "true",
		"io.runpool.instance": instance,
		"io.runpool.role":     "dind-data",
	}
	_, err := c.CreateVolume(ctx, name, own)
	if !errors.Is(err, engine.ErrAlreadyExists) {
		t.Fatalf("creating over an existing volume = %v; want ErrAlreadyExists", err)
	}
	// And the volume that was there is untouched: the labels are still
	// its owner's, so nothing read this as an adoption.
	if _, err := c.CreateVolume(ctx, name, own); !errors.Is(err, engine.ErrAlreadyExists) {
		t.Fatalf("the second create = %v; want the same refusal", err)
	}
}

// TestCacheLaneVolumes is the live cache contract, end to end through
// the real manager and store against the real daemon: warm data left by
// one job is what the next job of the same lane mounts; another
// generation is blind to it; eviction removes exactly the evicted
// lane's volume. Because a lane is a daemon-side named volume, where the
// controller runs cannot change the object mounted by a capsule.
func TestCacheLaneVolumes(t *testing.T) {
	c, instance := client(t)
	ctx := t.Context()

	st, err := store.Open(t.TempDir(), store.DefaultRetryBudget)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	mgr := cache.New(st, c, assignment.InstanceID(instance))

	release := func(lease string) {
		t.Helper()
		if err := st.Tx(ctx, func(tx *store.Tx) error { return tx.ReleaseCacheLane(assignment.LeaseID(lease)) }); err != nil {
			t.Fatal(err)
		}
	}
	acquire := func(gen, lease string) cache.LaneMount {
		t.Helper()
		loc, ok, err := mgr.Acquire(ctx, "https://github.com/acme/app", gen, assignment.LeaseID(lease), 2)
		if err != nil || !ok {
			t.Fatalf("acquire %s/%s: ok=%v, %v", gen, lease, ok, err)
		}
		t.Cleanup(func() { _ = c.RemoveVolume(context.Background(), loc.Volume) })
		return loc
	}
	// job runs a container with the lane mounted whole at /cache, the
	// way a capsule mounts it.
	job := func(name, volume, cmd string) (int64, string) {
		t.Helper()
		code, out, err := c.RunTask(ctx, engine.ContainerSpec{
			Name:   instance + "-" + name,
			Image:  busybox,
			Cmd:    []string{"sh", "-c", cmd},
			Labels: labels(instance, "lease-"+name, "task", "helper"),
			Mounts: []engine.Mount{{Volume: volume, Target: "/cache"}},
		})
		if err != nil {
			t.Fatalf("job %s: %v", name, err)
		}
		return code, out
	}

	first := acquire("gen-1", "lease-1")
	if code, out := job("write", first.Volume, "echo run-1 > /cache/marker"); code != 0 {
		t.Fatalf("writer exited %d: %s", code, out)
	}
	release("lease-1")

	// The same repository and generation reuses the lane, and the
	// marker written by the first job is exactly what the second sees.
	second := acquire("gen-1", "lease-2")
	if second.Volume != first.Volume {
		t.Fatalf("same repo+generation got volume %q; want %q reused", second.Volume, first.Volume)
	}
	code, out := job("read", second.Volume, "cat /cache/marker")
	if code != 0 || strings.TrimSpace(out) != "run-1" {
		t.Fatalf("the second job did not see the first's data: exit %d, %q", code, out)
	}
	release("lease-2")

	// Another generation gets its own lane and finds nothing in it.
	other := acquire("gen-2", "lease-3")
	if other.Volume == first.Volume {
		t.Fatalf("generation gen-2 was handed gen-1's volume %q", other.Volume)
	}
	if code, _ := job("blind", other.Volume, "test -e /cache/marker"); code == 0 {
		t.Fatal("another generation can see the first lane's data")
	}
	release("lease-3")

	// Eviction removes exactly the evicted lane's volume.
	if err := mgr.DeleteLane(ctx, first.LaneID); err != nil {
		t.Fatal(err)
	}
	gone, err := c.OwnedIDByName(ctx, "volume", first.Volume, assignment.InstanceID(instance), "")
	if err != nil || gone != "" {
		t.Errorf("evicted volume still resolves: %q, %v", gone, err)
	}
	left, err := c.OwnedIDByName(ctx, "volume", other.Volume, assignment.InstanceID(instance), "")
	if err != nil || left == "" {
		t.Errorf("the surviving lane's volume is gone: %q, %v", left, err)
	}
}

// TestFilesystemProbe is the disk monitor's measurement contract: the
// probe answers for the daemon's storage filesystem from inside it,
// with sane numbers, wherever the controller runs.
func TestFilesystemProbe(t *testing.T) {
	c, instance := client(t)

	free, err := c.ProbeFilesystemFree(t.Context(), busybox, assignment.InstanceID(instance))
	if err != nil {
		t.Fatal(err)
	}
	if free.FreeBytes <= 0 {
		t.Errorf("FreeBytes = %d; the daemon's filesystem is not empty", free.FreeBytes)
	}
	if free.FreeInodes == 0 {
		t.Errorf("FreeInodes = 0; want a positive count or -1 for a filesystem that does not account inodes")
	}
}

// TestOwnedVolumeUsage: the GC planner weighs lanes with the daemon's
// own accounting; a volume with data in it must report its size and
// carry its labels through.
func TestOwnedVolumeUsage(t *testing.T) {
	c, instance := client(t)
	ctx := t.Context()

	name := instance + "-usage"
	t.Cleanup(func() { _ = c.RemoveVolume(context.Background(), name) })
	if _, err := c.CreateVolume(ctx, name, labels(instance, "", "volume", cache.RoleCacheLane)); err != nil {
		t.Fatal(err)
	}
	code, out, err := c.RunTask(ctx, engine.ContainerSpec{
		Name:   instance + "-usage-writer",
		Image:  busybox,
		Cmd:    []string{"sh", "-c", "dd if=/dev/zero of=/v/data bs=1024 count=512 2>/dev/null"},
		Labels: labels(instance, "lease-usage", "task", "helper"),
		Mounts: []engine.Mount{{Volume: name, Target: "/v"}},
	})
	if err != nil || code != 0 {
		t.Fatalf("writer: exit %d, %v, %s", code, err, out)
	}

	usage, err := c.OwnedVolumeUsage(ctx, assignment.InstanceID(instance))
	if err != nil {
		t.Fatal(err)
	}
	var found *engine.VolumeUsage
	for i := range usage {
		if usage[i].Name == name {
			found = &usage[i]
		}
	}
	if found == nil {
		t.Fatalf("volume %q not in usage %+v", name, usage)
	}
	if found.Size < 512*1024 {
		t.Errorf("size = %d; want at least the 512KiB written", found.Size)
	}
	if found.Role != cache.RoleCacheLane {
		t.Errorf("labels did not travel through disk usage: %v", found.Labels)
	}
}

// TestContainerDiskFull: a job that fills its filesystem must fail with
// a clean out-of-space error inside the container, and the daemon must
// stay healthy and keep running other work — the host-level failure the
// disk monitor exists to prevent must not be reachable from inside one
// capped mount.
func TestContainerDiskFull(t *testing.T) {
	c, instance := client(t)
	ctx := t.Context()

	code, out, err := c.RunTask(ctx, engine.ContainerSpec{
		Name:   instance + "-diskfull",
		Image:  busybox,
		Cmd:    []string{"sh", "-c", "dd if=/dev/zero of=/scratch/fill bs=1M count=64"},
		Labels: labels(instance, "lease-diskfull", "task", "helper"),
		Tmpfs:  map[string]string{"/scratch": "rw,size=8m"},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if code == 0 {
		t.Fatal("64MiB fit in an 8MiB mount; the cap was not applied")
	}
	if !strings.Contains(strings.ToLower(out), "no space left") {
		t.Errorf("output %q; want a clean no-space-left error", out)
	}

	// The daemon is unharmed: it answers and runs the next task.
	if _, err := c.Info(ctx); err != nil {
		t.Fatalf("daemon after disk-full: %v", err)
	}
	code, _, err = c.RunTask(ctx, engine.ContainerSpec{
		Name:   instance + "-after-diskfull",
		Image:  busybox,
		Cmd:    []string{"true"},
		Labels: labels(instance, "lease-diskfull", "task", "helper"),
	})
	if err != nil || code != 0 {
		t.Fatalf("task after disk-full: exit %d, %v", code, err)
	}
}

// TestCleanupSurvivesCancellation is why RunTask holds its own deadline:
// a cancelled job must not cancel its own cleanup, or every timeout
// leaks a helper container until some later sweep.
func TestCleanupSurvivesCancellation(t *testing.T) {
	c, instance := client(t)

	var cleanupErr error
	c.OnCleanupError(func(string, error) { cleanupErr = errors.New("cleanup reported a failure") })

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	_, _, err := c.RunTask(ctx, engine.ContainerSpec{
		Name:   instance + "-cancelled",
		Image:  busybox,
		Cmd:    []string{"sleep", "60"},
		Labels: labels(instance, "lease-cancelled", "task", "helper"),
	})
	if err == nil {
		t.Fatal("a task outliving its context should fail")
	}
	if cleanupErr != nil {
		t.Errorf("%v; the removal deadline is not independent of the job context", cleanupErr)
	}

	// The helper is gone despite the cancellation.
	left, err := c.ListOwnedContainers(context.Background(), assignment.InstanceID(instance))
	if err != nil {
		t.Fatalf("list after cancellation: %v", err)
	}
	if len(left) != 0 {
		t.Errorf("%d helper(s) left behind after a cancelled task: %+v", len(left), left)
	}
}

func mustRemoveContainer(t *testing.T, c *docker.Client, ctx context.Context, id string) {
	t.Helper()
	cctx := context.WithoutCancel(ctx)
	if err := c.RemoveContainer(cctx, id); err != nil {
		t.Errorf("cleanup: container %s was not removed: %v", id, err)
		return
	}
	// Absence is verified, not assumed: a removal the daemon accepted
	// but did not complete leaves a privileged leftover this suite
	// exists to notice.
	if _, err := c.ContainerStatus(cctx, id); !errors.Is(err, engine.ErrNotFound) {
		t.Errorf("cleanup: container %s still exists after removal (status error: %v)", id, err)
	}
}

// TestContainerRemovalSparesNamedVolumes. Removal takes a container's
// anonymous volumes with it — an image that declares VOLUME grows one
// per container wherever a mount does not cover the path, and nothing
// labelled ever finds them. The half with data in it is the one this
// suite has to pin: a named volume mounted into the container is not
// touched by that flag.
func TestContainerRemovalSparesNamedVolumes(t *testing.T) {
	c, instance := client(t)
	ctx := t.Context()

	named := instance + "-keep"
	if _, err := c.CreateVolume(ctx, named, map[string]string{
		"io.runpool.managed":  "true",
		"io.runpool.instance": instance,
	}); err != nil {
		t.Fatalf("create named volume: %v", err)
	}
	t.Cleanup(func() { _ = c.RemoveVolume(context.Background(), named) })

	id, err := c.CreateContainer(ctx, engine.ContainerSpec{
		Name:  instance + "-anonvol",
		Image: busybox,
		Cmd:   []string{"true"},
		Labels: map[string]string{
			"io.runpool.managed":  "true",
			"io.runpool.instance": instance,
		},
		Mounts: []engine.Mount{
			{Volume: named, Target: "/data"},
			{Volume: "", Target: "/anon"},
		},
	})
	if err != nil {
		t.Fatalf("create container: %v", err)
	}
	t.Cleanup(func() { _ = c.RemoveContainer(context.Background(), id) })
	if err := c.RemoveContainer(ctx, id); err != nil {
		t.Fatalf("remove container: %v", err)
	}

	// Removing the named volume now must succeed, which is what proves
	// the container's removal left it standing.
	if err := c.RemoveVolume(ctx, named); err != nil {
		t.Fatalf("the named volume did not survive the container's removal: %v", err)
	}
}
