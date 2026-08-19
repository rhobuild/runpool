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
	"github.com/rhobuild/runpool/internal/platform/docker"

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

	uplinkID, err := dock.CreateNetwork(ctx, docker.NetworkSpec{
		Name:   leaseID + "-uplink",
		Labels: map[string]string{docker.LabelManaged: "true", docker.LabelInstance: "contract", docker.LabelRole: capsule.RoleUplink},
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
		Memory: 2 << 30,
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
	gwID, err := dock.OwnedIDByName(ctx, "container", "runpool-"+capsule.RoleGateway+"-"+short, "contract", assignment.LeaseID(leaseID))
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
