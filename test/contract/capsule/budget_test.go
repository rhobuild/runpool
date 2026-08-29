package capsulecontract

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rhobuild/runpool/internal/capsule"
	"github.com/rhobuild/runpool/internal/config"
	"github.com/rhobuild/runpool/internal/egress"
	"github.com/rhobuild/runpool/internal/engine"
	"github.com/rhobuild/runpool/internal/engine/docker"

	"github.com/rhobuild/runpool/internal/assignment"
)

// TestLeaseResourceBudget proves on a real kernel what the threat model
// claims: a lease cannot spend more than its tier, and that includes
// its gateway.
//
// The gateway is workload-driven — every connection a job opens is work
// it performs — so leaving it outside the envelope would let a job
// consume host resources its tier never granted. This asserts the three
// things that make the claim true: both containers sit under one parent
// cgroup, the sum of their kernel-enforced limits is the tier, and the
// gateway's own limits are the reserve the capsule gave up.
func TestLeaseResourceBudget(t *testing.T) {
	m, dock, leaseID := launcher(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()

	info, err := dock.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}

	rec := &memRecorder{}

	uplinkID, err := dock.CreateNetwork(ctx, engine.NetworkSpec{
		Name:   leaseID + "-uplink",
		Labels: engine.Ownership{Instance: "contract", Role: engine.RoleUplink}.Labels(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cleanupSandbox(t, dock, rec, uplinkID) })
	uplinkSubnet, err := dock.NetworkSubnet(ctx, uplinkID)
	if err != nil {
		t.Fatal(err)
	}

	tier := config.Resources{
		Memory: 768 << 20,
		Swap:   256 << 20,
		CPU:    2_000_000_000,
		PIDs:   512,
	}
	prepared, err := m.Prepare(ctx, capsule.Spec{
		LeaseID:      assignment.LeaseID(leaseID),
		InstanceID:   "contract",
		CapsuleImage: image,
		JITConfig:    fakeJITConfig,
		Resources:    tier,
		CgroupDriver: info.CgroupDriver,
		Sandbox: &capsule.Sandbox{
			UplinkNetworkID: uplinkID,
			UplinkSubnet:    uplinkSubnet,
			Deny:            egress.BuildDeny(uplinkSubnet, []string{"192.168.0.0/16"}, nil),
		},
	}, rec)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}

	short := leaseID
	if len(short) > 12 {
		short = short[:12]
	}
	gwID, err := dock.OwnedIDByName(ctx, engine.KindContainer, "runpool-"+string(engine.RoleGateway)+"-"+short, "contract", assignment.LeaseID(leaseID))
	if err != nil || gwID == "" {
		t.Fatalf("resolve gateway: %q, %v", gwID, err)
	}

	// Both containers report the same parent cgroup, so the kernel
	// accounts them as one aggregate and one path reports the lease.
	wantParent := capsule.LeaseCgroupParent(info.CgroupDriver, leaseID)
	for name, id := range map[string]string{"capsule": string(prepared.RuntimeID), "gateway": gwID} {
		got, err := dock.ContainerCgroupParent(ctx, id)
		if err != nil {
			t.Fatalf("%s cgroup parent: %v", name, err)
		}
		if got != wantParent {
			t.Errorf("%s parent cgroup = %q; want %q", name, got, wantParent)
		}
	}

	// The kernel's own numbers, read from inside each container's
	// cgroup: what the daemon was asked for is not evidence, what the
	// kernel enforces is.
	capsuleLimits := readCgroupLimits(ctx, t, dock, string(prepared.RuntimeID))
	gatewayLimits := readCgroupLimits(ctx, t, dock, gwID)

	if got := capsuleLimits.memory + gatewayLimits.memory; got != int64(tier.Memory) {
		t.Errorf("memory: capsule %d + gateway %d = %d; the tier is %d — the lease can exceed its budget",
			capsuleLimits.memory, gatewayLimits.memory, got, int64(tier.Memory))
	}
	if got := capsuleLimits.swap + gatewayLimits.swap; got != int64(tier.Swap) {
		t.Errorf("swap: capsule %d + gateway %d = %d; the tier is %d",
			capsuleLimits.swap, gatewayLimits.swap, got, int64(tier.Swap))
	}
	if got := capsuleLimits.pids + gatewayLimits.pids; got != tier.PIDs {
		t.Errorf("pids: capsule %d + gateway %d = %d; the tier is %d",
			capsuleLimits.pids, gatewayLimits.pids, got, tier.PIDs)
	}
	if gatewayLimits.memory != int64(config.GatewayReserveMemory) {
		t.Errorf("gateway memory = %d; want the reserve %d", gatewayLimits.memory, config.GatewayReserveMemory)
	}
	if gatewayLimits.pids != config.GatewayReservePIDs {
		t.Errorf("gateway pids = %d; want the reserve %d", gatewayLimits.pids, config.GatewayReservePIDs)
	}

	// The gateway's limits are enforced, not advisory. A fork storm
	// inside it must stop at its own ceiling rather than reaching the
	// tier, let alone the host's process table.
	//
	// The storm runs detached and the evidence is read by a separate
	// exec: a shell that has exhausted its own PID budget cannot fork
	// the process that would report the result, so asking it to would
	// measure nothing. The first attempt at this test did exactly that
	// and passed while proving nothing.
	if _, _, err := dock.Exec(ctx, gwID, []string{"bash", "-c",
		`(n=0; while [ $n -lt 4000 ]; do sleep 20 & n=$((n+1)); done) >/dev/null 2>&1 &`}); err != nil {
		t.Fatalf("start the fork storm: %v", err)
	}
	time.Sleep(3 * time.Second)

	code, out, err := dock.Exec(ctx, gwID, []string{"cat", "/sys/fs/cgroup/pids.current"})
	if err != nil || code != 0 {
		t.Fatalf("read pids.current: exit %d, %v", code, err)
	}
	current, perr := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if perr != nil {
		t.Fatalf("pids.current is %q: %v", strings.TrimSpace(out), perr)
	}
	t.Logf("gateway pids.current under a fork storm: %d (ceiling %d, tier %d)",
		current, config.GatewayReservePIDs, tier.PIDs)

	// The pids controller blocks fork, not migration: a `docker exec`
	// attaches its process to the cgroup without passing the limit, so
	// the reader itself appears above the ceiling. Measured here as
	// exactly one over. The allowance covers the reader and its shell,
	// and stays far below the tier — which is the claim that matters:
	// the gateway cannot spend the lease's whole budget.
	const execAllowance = 4
	if current > config.GatewayReservePIDs+execAllowance {
		t.Errorf("the gateway holds %d processes; its ceiling is %d — the limit is not enforced",
			current, config.GatewayReservePIDs)
	}
	if current >= tier.PIDs {
		t.Errorf("the gateway reached %d processes, the tier's whole budget of %d", current, tier.PIDs)
	}
	if _, err := dock.ContainerStatus(ctx, gwID); err != nil {
		t.Errorf("the gateway did not survive its own PID ceiling: %v", err)
	}

	// Limits written into cgroup files are configuration evidence. Force
	// anonymous-like tmpfs pages past memory.max and require the kernel to
	// account non-zero swap before calling the swap envelope enforced.
	const pressureImage = "busybox:1.36@sha256:73aaf090f3d85aa34ee199857f03fa3a95c8ede2ffd4cc2cdb5b94e566b11662"
	outerID := string(prepared.RuntimeID)
	innerDocker := func(arguments ...string) (int, string, error) {
		command := append([]string{"docker", "--host=" + innerDockerSocket}, arguments...)
		return dock.Exec(ctx, outerID, command)
	}
	if code, out, err := innerDocker("pull", pressureImage); err != nil || code != 0 {
		t.Fatalf("pull the locked memory-pressure image through the inner daemon: exit %d, %v: %s", code, err, out)
	}
	pressureName := "runpool-contract-pressure-" + short
	t.Cleanup(func() { _, _, _ = innerDocker("rm", "-f", pressureName) })
	memoryMiB := capsuleLimits.memory >> 20
	code, out, err = innerDocker("run", "-d", "--name", pressureName,
		"--tmpfs", "/pressure:rw,size="+strconv.FormatInt(memoryMiB+16, 10)+"m",
		pressureImage, "sh", "-c",
		"dd if=/dev/zero of=/pressure/blob bs=1M count="+strconv.FormatInt(memoryMiB, 10)+
			" status=none && touch /pressure/ready && sleep 120")
	if err != nil || code != 0 {
		t.Fatalf("start memory pressure: exit %d, %v: %s", code, err, out)
	}
	waitForInnerFile(t, ctx, innerDocker, pressureName, "/pressure/ready")
	var swapCurrent int64
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		swapCurrent = readCgroupCounter(ctx, t, dock, outerID, "memory.swap.current", "")
		if swapCurrent > 0 {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if swapCurrent == 0 {
		t.Fatal("memory.swap.current remained zero after memory pressure exceeded memory.max; configured swap was not exercised")
	}
	t.Logf("capsule used %d bytes of swap under %d bytes of memory pressure", swapCurrent, capsuleLimits.memory)
	if code, out, err := innerDocker("rm", "-f", pressureName); err != nil || code != 0 {
		t.Fatalf("release memory pressure: exit %d, %v: %s", code, err, out)
	}

	// An inner workload that exhausts the aggregate envelope must be charged
	// to the capsule's cgroup. Mark the pressure process as the preferred OOM
	// victim so the property under test is deterministic: the workload dies,
	// while supervisor and daemon remain available to report the hierarchical
	// memory.events counter.
	oomBefore := readCgroupCounter(ctx, t, dock, outerID, "memory.events", "oom_kill")
	totalMiB := (capsuleLimits.memory + capsuleLimits.swap) >> 20
	code, out, err = innerDocker("run", "--rm",
		"--tmpfs", "/pressure:rw,size="+strconv.FormatInt(totalMiB+256, 10)+"m",
		pressureImage, "sh", "-c",
		"echo 1000 > /proc/self/oom_score_adj && exec dd if=/dev/zero of=/pressure/blob bs=1M count="+
			strconv.FormatInt(totalMiB+128, 10)+" status=none")
	if err != nil {
		t.Fatalf("drive the inner OOM boundary: %v", err)
	}
	if code == 0 {
		t.Fatalf("an inner workload wrote past memory+swap without being killed: %s", out)
	}
	state, err := dock.ContainerStatus(ctx, outerID)
	if err != nil {
		t.Fatalf("inspect the capsule after inner OOM: %v", err)
	}
	if state.Status == engine.StatusRunning {
		oomAfter := readCgroupCounter(ctx, t, dock, outerID, "memory.events", "oom_kill")
		if oomAfter <= oomBefore {
			t.Fatalf("inner workload exited %d but capsule oom_kill stayed at %d; the failure was not attributed to this cgroup: %s",
				code, oomBefore, out)
		}
		t.Logf("capsule oom_kill increased from %d to %d after inner workload exit %d", oomBefore, oomAfter, code)
	} else if !state.OOMKilled || state.ExitCode != 137 {
		t.Fatalf("capsule stopped after inner pressure as status=%s exit=%d oom_killed=%t; require an attributed kernel OOM",
			state.Status, state.ExitCode, state.OOMKilled)
	} else {
		t.Logf("kernel attributed the inner pressure to the capsule: status=%s exit=%d oom_killed=%t",
			state.Status, state.ExitCode, state.OOMKilled)
	}
}

type cgroupLimits struct {
	memory int64
	swap   int64
	pids   int64
}

// readCgroupLimits reads the limits from inside the container's own
// cgroup namespace, where cgroup v2 exposes them at the root.
func readCgroupLimits(ctx context.Context, t *testing.T, dock *docker.Client, id string) cgroupLimits {
	t.Helper()
	read := func(file string) int64 {
		code, out, err := dock.Exec(ctx, id, []string{"cat", "/sys/fs/cgroup/" + file})
		if err != nil || code != 0 {
			t.Fatalf("read %s from %s: exit %d, %v", file, id[:12], code, err)
		}
		text := strings.TrimSpace(out)
		if text == "max" {
			t.Fatalf("%s is unlimited in %s; the envelope was not applied", file, id[:12])
		}
		n, perr := strconv.ParseInt(text, 10, 64)
		if perr != nil {
			t.Fatalf("%s in %s is %q: %v", file, id[:12], text, perr)
		}
		return n
	}
	return cgroupLimits{
		memory: read("memory.max"),
		swap:   read("memory.swap.max"),
		pids:   read("pids.max"),
	}
}

func readCgroupCounter(ctx context.Context, t *testing.T, dock *docker.Client, id, file, key string) int64 {
	t.Helper()
	code, out, err := dock.Exec(ctx, id, []string{"cat", "/sys/fs/cgroup/" + file})
	if err != nil || code != 0 {
		t.Fatalf("read %s from %s: exit %d, %v", file, id[:12], code, err)
	}
	if key == "" {
		value, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
		if err != nil {
			t.Fatalf("%s in %s is %q: %v", file, id[:12], strings.TrimSpace(out), err)
		}
		return value
	}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != key {
			continue
		}
		value, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			t.Fatalf("%s %s in %s is %q: %v", file, key, id[:12], fields[1], err)
		}
		return value
	}
	t.Fatalf("%s in %s has no %s counter: %s", file, id[:12], key, out)
	return 0
}

func waitForInnerFile(t *testing.T, ctx context.Context,
	innerDocker func(...string) (int, string, error), container, path string,
) {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		code, _, err := innerDocker("exec", container, "test", "-f", path)
		if err == nil && code == 0 {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	code, out, err := innerDocker("logs", container)
	t.Fatalf("inner pressure did not reach %s: logs exit %d, %v: %s", path, code, err, out)
}
