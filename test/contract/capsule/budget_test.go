package capsulecontract

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rhobuild/runpool/internal/assignment"
	"github.com/rhobuild/runpool/internal/capsule"
	"github.com/rhobuild/runpool/internal/config"
	"github.com/rhobuild/runpool/internal/egress"
	"github.com/rhobuild/runpool/internal/engine"
	"github.com/rhobuild/runpool/internal/engine/docker"
)

const pressureImage = "busybox:1.36@sha256:73aaf090f3d85aa34ee199857f03fa3a95c8ede2ffd4cc2cdb5b94e566b11662"

type resourceContractFixture struct {
	ctx           context.Context
	dock          *docker.Client
	leaseID       string
	shortLeaseID  string
	capsuleID     string
	gatewayID     string
	cgroupDriver  string
	architecture  string
	tier          config.Resources
	capsuleLimits cgroupLimits
	gatewayLimits cgroupLimits
}

func newResourceContractFixture(t *testing.T) *resourceContractFixture {
	t.Helper()
	m, dock, leaseID := launcher(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	t.Cleanup(cancel)

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
	info, err := dock.Info(ctx)
	if err != nil {
		t.Fatalf("read daemon facts: %v", err)
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

	capsuleID := string(prepared.RuntimeID)
	return &resourceContractFixture{
		ctx:           ctx,
		dock:          dock,
		leaseID:       leaseID,
		shortLeaseID:  short,
		capsuleID:     capsuleID,
		gatewayID:     gwID,
		cgroupDriver:  info.CgroupDriver,
		architecture:  info.Architecture,
		tier:          tier,
		capsuleLimits: readCgroupLimits(ctx, t, dock, capsuleID),
		gatewayLimits: readCgroupLimits(ctx, t, dock, gwID),
	}
}

func (f *resourceContractFixture) innerDocker(arguments ...string) (int, string, error) {
	command := append([]string{"docker", "--host=" + innerDockerSocket}, arguments...)
	return f.dock.Exec(f.ctx, f.capsuleID, command)
}

func installPIDPressureHelper(t *testing.T, f *resourceContractFixture) {
	t.Helper()
	binary := linuxContractBinary(t, f)
	input, err := os.ReadFile(binary)
	if err != nil {
		t.Fatalf("open PID pressure helper: %v", err)
	}
	command := []string{"bash", "-c", "umask 077; cat > " + pidPressureBinaryPath +
		" && chmod 700 " + pidPressureBinaryPath}
	if code, out, err := f.dock.ExecWithInput(f.ctx, f.gatewayID, command, input); err != nil || code != 0 {
		t.Fatalf("install PID pressure helper: exit %d, %v: %s", code, err, out)
	}
}

func linuxContractBinary(t *testing.T, f *resourceContractFixture) string {
	t.Helper()
	architecture := f.architecture
	switch architecture {
	case "amd64", "x86_64":
		architecture = "amd64"
	case "arm64", "aarch64":
		architecture = "arm64"
	default:
		t.Fatalf("Docker reports architecture %q; the contract cannot build a Linux helper for it", f.architecture)
	}
	if runtime.GOOS == "linux" && runtime.GOARCH == architecture {
		binary, err := os.Executable()
		if err != nil {
			t.Fatalf("resolve contract executable: %v", err)
		}
		return binary
	}

	repository, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}
	binary := filepath.Join(t.TempDir(), "capsule-contract.test")
	command := exec.CommandContext(f.ctx, "go", "test", "-c", "-o", binary, "./test/contract/capsule")
	command.Dir = repository
	command.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+architecture, "CGO_ENABLED=0")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build Linux PID pressure helper: %v\n%s", err, output)
	}
	return binary
}

func waitForGatewayMarker(t *testing.T, f *resourceContractFixture, marker string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		code, _, err := f.dock.Exec(f.ctx, f.gatewayID, []string{"bash", "-c", "test -f " + marker})
		if err == nil && code == 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	code, output, err := f.dock.Exec(f.ctx, f.gatewayID, []string{"cat", pidPressureLogPath})
	t.Fatalf("PID pressure helper did not publish %s: log exit %d, %v: %s", marker, code, err, output)
}

// TestLeaseResourceEnvelopeAndGatewayPIDRecovery proves that a lease cannot
// spend more than its tier, including gateway work, and that enforcing the
// gateway's PID reserve does not leave the relay unavailable after pressure
// subsides.
func TestLeaseResourceEnvelopeAndGatewayPIDRecovery(t *testing.T) {
	f := newResourceContractFixture(t)

	// Both containers report the same parent cgroup, so the kernel
	// accounts them as one aggregate and one path reports the lease.
	wantParent := capsule.LeaseCgroupParent(f.cgroupDriver, f.leaseID)
	for name, id := range map[string]string{"capsule": f.capsuleID, "gateway": f.gatewayID} {
		got, err := f.dock.ContainerCgroupParent(f.ctx, id)
		if err != nil {
			t.Fatalf("%s cgroup parent: %v", name, err)
		}
		if got != wantParent {
			t.Errorf("%s parent cgroup = %q; want %q", name, got, wantParent)
		}
	}

	if got := f.capsuleLimits.memory + f.gatewayLimits.memory; got != int64(f.tier.Memory) {
		t.Errorf("memory: capsule %d + gateway %d = %d; the tier is %d — the lease can exceed its budget",
			f.capsuleLimits.memory, f.gatewayLimits.memory, got, int64(f.tier.Memory))
	}
	if got := f.capsuleLimits.swap + f.gatewayLimits.swap; got != int64(f.tier.Swap) {
		t.Errorf("swap: capsule %d + gateway %d = %d; the tier is %d",
			f.capsuleLimits.swap, f.gatewayLimits.swap, got, int64(f.tier.Swap))
	}
	if got := f.capsuleLimits.pids + f.gatewayLimits.pids; got != f.tier.PIDs {
		t.Errorf("pids: capsule %d + gateway %d = %d; the tier is %d",
			f.capsuleLimits.pids, f.gatewayLimits.pids, got, f.tier.PIDs)
	}
	if f.gatewayLimits.memory != int64(config.GatewayReserveMemory) {
		t.Errorf("gateway memory = %d; want the reserve %d", f.gatewayLimits.memory, config.GatewayReserveMemory)
	}
	if f.gatewayLimits.pids != config.GatewayReservePIDs {
		t.Errorf("gateway pids = %d; want the reserve %d", f.gatewayLimits.pids, config.GatewayReservePIDs)
	}
	installPIDPressureHelper(t, f)
	idleGatewayPIDs := readCgroupCounter(f.ctx, t, f.dock, f.gatewayID, "pids.current", "")

	// The helper starts children until the kernel returns EAGAIN, records the
	// cgroup counter without introducing another process, holds the boundary for
	// two seconds, and reaps every child before publishing its evidence. A shell
	// loop is not suitable here: Bash retries failed forks and can turn a nominal
	// process count into an unbounded pressure duration.
	startPressure := "rm -f " + pidPressureDonePath + " " + pidPressureDonePath + ".tmp " +
		pidPressureLogPath + "; " +
		pidPressureHelperEnvironment + "=1 " + pidPressureBinaryPath +
		" >" + pidPressureLogPath + " 2>&1 &"
	if code, out, err := f.dock.Exec(f.ctx, f.gatewayID, []string{"bash", "-c", startPressure}); err != nil || code != 0 {
		t.Fatalf("start PID pressure helper: exit %d, %v: %s", code, err, out)
	}
	waitForGatewayMarker(t, f, pidPressureDonePath)

	code, out, err := f.dock.Exec(f.ctx, f.gatewayID, []string{"cat", pidPressureDonePath})
	if err != nil || code != 0 {
		t.Fatalf("read PID pressure evidence: exit %d, %v: %s", code, err, out)
	}
	var evidence pidPressureEvidence
	if err := json.Unmarshal([]byte(out), &evidence); err != nil {
		t.Fatalf("decode PID pressure evidence %q: %v", strings.TrimSpace(out), err)
	}
	t.Logf("gateway started %d pressure processes and observed pids.current=%d (ceiling %d, tier %d)",
		evidence.Spawned, evidence.Saturated, config.GatewayReservePIDs, f.tier.PIDs)
	if !evidence.LimitReached {
		t.Error("PID pressure helper published evidence without observing EAGAIN")
	}
	if evidence.Spawned <= 0 || evidence.Spawned >= config.GatewayReservePIDs {
		t.Errorf("PID pressure helper started %d children under a reserve of %d", evidence.Spawned, config.GatewayReservePIDs)
	}
	if evidence.Saturated != config.GatewayReservePIDs {
		t.Errorf("gateway observed pids.current=%d at EAGAIN; want its exact ceiling %d",
			evidence.Saturated, config.GatewayReservePIDs)
	}
	if evidence.Saturated >= f.tier.PIDs {
		t.Errorf("the gateway reached %d processes, the tier's whole budget of %d", evidence.Saturated, f.tier.PIDs)
	}
	// The helper still contributes its Go runtime threads when it records this
	// counter. Requiring it to fall below one quarter of the reserve proves the
	// pressure children were reaped; the external measurement below proves the
	// final return to the gateway's own idle level after the helper exits.
	if evidence.Recovered >= config.GatewayReservePIDs/4 {
		t.Errorf("helper reaped its children but pids.current remained at %d under a reserve of %d",
			evidence.Recovered, config.GatewayReservePIDs)
	}
	if _, err := f.dock.ContainerStatus(f.ctx, f.gatewayID); err != nil {
		t.Errorf("the gateway did not survive its own PID ceiling: %v", err)
	}

	// docker exec adds its reader to the cgroup. Allow only that bounded
	// measurement overhead when confirming recovery from outside the helper.
	const execAllowance = 4
	recoveryDeadline := time.Now().Add(30 * time.Second)
	for {
		recovered := readCgroupCounter(f.ctx, t, f.dock, f.gatewayID, "pids.current", "")
		if recovered <= idleGatewayPIDs+execAllowance {
			break
		}
		if time.Now().After(recoveryDeadline) {
			t.Fatalf("gateway pids.current remained at %d after the fork storm; idle was %d",
				recovered, idleGatewayPIDs)
		}
		time.Sleep(250 * time.Millisecond)
	}
	proxyProbe := `h=${http_proxy#http://}; h=${h%%:*}; timeout 5 bash -c "echo > /dev/tcp/$h/3128"`
	if code, out, err := f.dock.Exec(f.ctx, f.capsuleID, []string{"bash", "-c", proxyProbe}); err != nil || code != 0 {
		t.Fatalf("gateway proxy did not recover after PID pressure: exit %d, %v: %s", code, err, out)
	}
}

// TestCapsuleSwapAndOOMStayWithinTheLeaseEnvelope uses a fresh lease so PID
// pressure in another contract cannot determine whether the memory fixture has
// egress or whether the kernel accounts swap and OOM to this capsule.
func TestCapsuleSwapAndOOMStayWithinTheLeaseEnvelope(t *testing.T) {
	f := newResourceContractFixture(t)
	if code, out, err := f.innerDocker("pull", pressureImage); err != nil || code != 0 {
		t.Fatalf("pull the locked memory-pressure image through the inner daemon: exit %d, %v: %s", code, err, out)
	}

	// Limits written into cgroup files are configuration evidence. Force
	// anonymous-like tmpfs pages past memory.max and require the kernel to
	// account non-zero swap before calling the swap envelope enforced.
	pressureName := "runpool-contract-pressure-" + f.shortLeaseID
	t.Cleanup(func() { _, _, _ = f.innerDocker("rm", "-f", pressureName) })
	memoryMiB := f.capsuleLimits.memory >> 20
	code, out, err := f.innerDocker("run", "-d", "--name", pressureName,
		"--tmpfs", "/pressure:rw,size="+strconv.FormatInt(memoryMiB+16, 10)+"m",
		pressureImage, "sh", "-c",
		"dd if=/dev/zero of=/pressure/blob bs=1M count="+strconv.FormatInt(memoryMiB, 10)+
			" status=none && touch /pressure/ready && sleep 120")
	if err != nil || code != 0 {
		t.Fatalf("start memory pressure: exit %d, %v: %s", code, err, out)
	}
	waitForInnerFile(t, f.ctx, f.innerDocker, pressureName, "/pressure/ready")
	var swapCurrent int64
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		swapCurrent = readCgroupCounter(f.ctx, t, f.dock, f.capsuleID, "memory.swap.current", "")
		if swapCurrent > 0 {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if swapCurrent == 0 {
		t.Fatal("memory.swap.current remained zero after memory pressure exceeded memory.max; configured swap was not exercised")
	}
	t.Logf("capsule used %d bytes of swap under %d bytes of memory pressure", swapCurrent, f.capsuleLimits.memory)
	if code, out, err := f.innerDocker("rm", "-f", pressureName); err != nil || code != 0 {
		t.Fatalf("release memory pressure: exit %d, %v: %s", code, err, out)
	}

	// An inner workload that exhausts the aggregate envelope must be charged
	// to the capsule's cgroup. Mark the pressure process as the preferred OOM
	// victim so the property under test is deterministic: the workload dies,
	// while supervisor and daemon remain available to report the hierarchical
	// memory.events counter.
	oomBefore := readCgroupCounter(f.ctx, t, f.dock, f.capsuleID, "memory.events", "oom_kill")
	totalMiB := (f.capsuleLimits.memory + f.capsuleLimits.swap) >> 20
	code, out, err = f.innerDocker("run", "--rm",
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
	state, err := f.dock.ContainerStatus(f.ctx, f.capsuleID)
	if err != nil {
		t.Fatalf("inspect the capsule after inner OOM: %v", err)
	}
	if state.Status == engine.StatusRunning {
		oomAfter := readCgroupCounter(f.ctx, t, f.dock, f.capsuleID, "memory.events", "oom_kill")
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
