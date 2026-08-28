// Package capsule orchestrates one per-job execution capsule: a single
// outer container that holds the runner, its own Docker daemon and
// every inner container the job launches, under one aggregate cgroup.
// One container means one budget the kernel enforces over the whole job
// — runner, daemon and inner workloads cannot escape into separate
// envelopes — and one object whose state answers what became of the
// work.
//
// Inside, the first-party supervisor (cmd/capsule-supervisor) is PID 1:
// it boots dockerd, proves readiness, holds the runner unstarted until
// the controller authorizes it, and reports through a versioned
// filesystem protocol on tmpfs. The daemon's data root is a fresh volume per
// capsule, and the complete capsule is deleted after the job, so no image,
// layer, build cache, or runner bootstrap state reaches a later workload.
package capsule

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/rhobuild/runpool/internal/capsule/protocol"
	"github.com/rhobuild/runpool/internal/config"
	"github.com/rhobuild/runpool/internal/egress"
	"github.com/rhobuild/runpool/internal/engine"

	"github.com/rhobuild/runpool/internal/assignment"
)

const (
	// SupervisorPath is where the capsule image installs the supervisor;
	// its subcommands are the control protocol's exec surface, used
	// from here and by the controller's own gateway reloads.
	SupervisorPath = "/usr/local/bin/capsule-supervisor"
	supervisorPath = SupervisorPath
	// controlDir is the tmpfs control surface, declared at creation so
	// nothing under it can reach a disk or a Docker-persisted field.
	controlDir = "/run/runpool"

	// Enough for the supervisor's own account of why it stopped, which is
	// the last thing it logs, without pasting a boot transcript into an
	// error an operator reads at a glance.
	logTailLines = 5
	// protocolFile is where the supervisor writes the version of the
	// control protocol it speaks, at boot, before it is asked anything.
	protocolFile = controlDir + "/protocol"
	// ProtocolVersion is the control protocol this build speaks. The
	// declaration itself lives in internal/capsule/protocol, imported by
	// the supervisor too, so the two sides cannot disagree.
	ProtocolVersion = protocol.Version
	// dindDataDir is the inner daemon's data root — the one mount that
	// must be a volume, because overlayfs cannot stack on the
	// container's own overlayfs.
	dindDataDir = "/var/lib/docker"

	readyTimeout = 90 * time.Second
	// protocolTimeout bounds the wait for a capsule to declare what it
	// speaks. The supervisor writes that file before any state, so a
	// first-party capsule answers within moments of starting; this is
	// the bound on "this is not a Runpool capsule", not on readiness.
	protocolTimeout   = 30 * time.Second
	readyPollInterval = 500 * time.Millisecond
)

// Resource kinds and roles stamped on every capsule object, mirrored
// into the resource intents so reconciliation can find and order them.
const ()

const (
	// GatewayProxyPort is where a sandboxed capsule's egress relay
	// listens; it is the capsule's whole outbound surface. The port is
	// named in the egress policy vocabulary both sides share, so this
	// package does not import the gateway to learn one integer.
	GatewayProxyPort = egress.ProxyPort
	// innerDockerCIDR is the default bridge the capsule's own daemon
	// creates. Traffic to it never leaves the capsule, so it must not
	// be sent to the relay.
	innerDockerCIDR = "172.17.0.0/16"
)

type Spec struct {
	LeaseID    assignment.LeaseID
	InstanceID assignment.InstanceID
	// AttemptID, TargetID and TierID are provider-neutral correlation
	// labels. They make ephemeral resources intelligible in a shared
	// daemon's container view without exposing credentials or source URLs.
	AttemptID assignment.AttemptID
	TargetID  assignment.TargetID
	TierID    assignment.TierID
	// CapsuleImage is the first-party outer image: supervisor, runner
	// and engine in one filesystem.
	CapsuleImage string
	JITConfig    string
	// Resources is the tier envelope, applied once to the outer
	// container: the aggregate cgroup is the whole point of it.
	Resources config.Resources
	// Cache, when set, is the repository cache lane mounted at /cache in
	// the capsule — its only persistent state. The supervisor makes it
	// writable by the runner uid at boot.
	Cache CacheMount
	// Sandbox, when set, encapsulates the capsule's network: internal
	// isolated bridge, per-capsule egress gateway on the instance
	// uplink, default-deny policy, gateway DNS. Nil is the
	// unsafe-open-egress profile: a plain bridge with host egress.
	Sandbox *Sandbox
	// CgroupDriver is the daemon's driver ("systemd" or "cgroupfs"). It
	// decides the form of the lease's parent cgroup, which the daemon
	// validates — a slice unit for systemd, a path for cgroupfs.
	CgroupDriver string
}

// Sandbox is the controller-computed sandbox input: where the uplink
// is, and the policy the gateway must install before the capsule may
// run. Deny is a complete snapshot — baseline ranges, host addresses,
// Docker subnets, the uplink itself; incomplete discovery must fail
// admission upstream, never launch with a partial deny set.
type Sandbox struct {
	UplinkNetworkID string
	UplinkSubnet    string
	Allow           []string
	Deny            []string
}

// CacheMount names a cache lane: one named volume, mounted whole. No
// path and no subpath exists to resolve, which is what makes the mount
// identical however the controller is deployed.
type CacheMount struct {
	Volume string
}

func (c CacheMount) empty() bool { return c.Volume == "" }

// CachePath is where a repository cache lane is mounted in the capsule;
// workflows point CARGO_HOME, and so on, beneath it.
const CachePath = "/cache"

// ResourceRecorder makes every external object durable around its
// creation, not after it. Plan commits the intent — kind, role and the
// deterministic name — before any effect; Creating marks the ambiguous
// window just before the create call; Confirm records the object's id
// once it exists. A crash anywhere leaves an intent whose name finds
// the object or proves its absence, which is why no compensation logic
// lives here anymore: the record cannot be lost, only unconfirmed.
type ResourceRecorder interface {
	Plan(kind, role, name string) (assignment.ResourceIntentID, error)
	Creating(intentID assignment.ResourceIntentID) error
	Confirm(intentID assignment.ResourceIntentID, dockerID string) error
}

// create wraps one object creation in its intent lifecycle. When the
// create call reports a name conflict, resolve settles it: the object
// is ours from an earlier incarnation of this same intent — adopted by
// proven ownership, never by name — or it is foreign and the launch
// fails closed.
func (m *Launcher) create(ctx context.Context, rec ResourceRecorder, kind engine.ObjectKind, role engine.Role, name string,
	createFn func() (string, error), resolve func() (string, error)) (string, error) {
	// The kind crosses as a string on purpose: the recorder is the lease
	// machine, which may not know a container runtime -- cleanup that
	// depends on one stops working exactly when the runtime is what
	// failed. It names the same vocabulary in its own type on the far
	// side, and this is the one place the two meet.
	intentID, err := rec.Plan(string(kind), string(role), name)
	if err != nil {
		return "", err
	}
	if err := rec.Creating(intentID); err != nil {
		return "", err
	}
	objectID, err := createFn()
	if err != nil {
		// Only a taken name is worth resolving, and only then is
		// adoption an answer rather than a guess. A create that failed
		// for any other reason -- the daemon gone, the image refused --
		// says nothing about whether an object exists, and looking one
		// up anyway reported the failure of the lookup under the name of
		// the create. The object is not lost either way: the intent is
		// already durable and in its creating state, so recovery
		// resolves it by name.
		if !errors.Is(err, engine.ErrAlreadyExists) {
			return "", fmt.Errorf("create %s %s: %w", kind, role, err)
		}
		existing, rerr := resolve()
		if rerr != nil {
			return "", fmt.Errorf("resolving the %s %s that already exists: %w", kind, role, rerr)
		}
		if existing == "" {
			// The name was taken when the create ran and free when the
			// lookup ran, so nothing is there to adopt.
			return "", fmt.Errorf("create %s %s: %w", kind, role, err)
		}
		objectID = existing
	}
	if err := rec.Confirm(intentID, objectID); err != nil {
		// The object exists and the intent still names it in its
		// creating state: recovery resolves it by name. Nothing is
		// removed here, because losing the object is worse than
		// re-finding it.
		return "", fmt.Errorf("confirming %s %s: %w", kind, role, err)
	}
	return objectID, nil
}

// capsuleDaemon is the daemon as a capsule needs it, declared here
// rather than by the adapter: what this package may know is the twelve
// operations a capsule is built from, and a seam is what lets the
// refusals be asked for. A live daemon cannot be told to be absent, to
// be unreachable, or to refuse a container that is already running, and
// those are the answers that decide whether an attempt is settled or
// held for a person.
type capsuleDaemon interface {
	CreateNetwork(context.Context, engine.NetworkSpec) (string, error)
	CreateVolume(ctx context.Context, name string, labels map[string]string) (string, error)
	CreateContainer(context.Context, engine.ContainerSpec) (string, error)
	StartContainer(ctx context.Context, id string) error
	ConnectNetwork(ctx context.Context, networkID, containerID string) error
	NetworkSubnet(ctx context.Context, id string) (string, error)
	ContainerIPOn(ctx context.Context, containerID, networkID string) (ip, subnet string, err error)
	ContainerStatus(ctx context.Context, id string) (engine.ContainerState, error)
	Exec(ctx context.Context, containerID string, cmd []string) (int, string, error)
	ExecWithInput(ctx context.Context, containerID string, cmd []string, input []byte) (int, string, error)
	OwnedIDByName(ctx context.Context, kind engine.ObjectKind, name string,
		instanceID assignment.InstanceID, leaseID assignment.LeaseID) (string, error)
	TailLogs(ctx context.Context, id string, lines int) (string, error)
}

// Launcher builds capsules. It holds no per-capsule state: everything a
// launch needs arrives in its Spec, and everything it creates is
// recorded through the caller's ResourceRecorder.
type Launcher struct {
	dock capsuleDaemon
	// gatewayImage is what every egress gateway runs, whatever a tier
	// configured for its jobs. It belongs to the launcher rather than to
	// a Spec because it is not a per-launch decision: the gateway is the
	// container that applies the policy confining a job, and a deployment
	// extending the image its jobs run in is not asking to replace that.
	// A Spec field would be one more thing a caller can leave empty, and
	// the caller who does is launching an unconfined job.
	gatewayImage string
}

func NewLauncher(d capsuleDaemon, gatewayImage string) *Launcher {
	return &Launcher{dock: d, gatewayImage: gatewayImage}
}

// PreparedRuntime is a capsule that exists but has not been asked to
// run: the outer container is up, its daemon is ready, the credential
// is delivered, and the supervisor holds the runner unstarted. It is
// the value that separates preparation from execution.
type PreparedRuntime struct {
	// RuntimeID is the outer container: the one object whose state —
	// and whose supervisor's state file — answers whether execution
	// ever began.
	RuntimeID assignment.RuntimeID
}

// Prepare builds the capsule and stops short of the one effect that can
// begin execution. The outer container runs from here — its daemon must
// boot and prove readiness — but the supervisor refuses to launch the
// runner until Start, so nothing the job could observe has happened. A
// failure anywhere in here is retriable by construction.
func (m *Launcher) Prepare(ctx context.Context, spec Spec, rec ResourceRecorder) (PreparedRuntime, error) {
	outerID, err := m.prepare(ctx, spec, rec)
	if err != nil {
		return PreparedRuntime{}, err
	}
	return PreparedRuntime{RuntimeID: assignment.RuntimeID(outerID)}, nil
}

// Start authorizes the runner. It is deliberately one exec that drops
// the start sentinel: the caller persists the authorization first, so
// the ambiguous window is exactly one request wide, and
// InspectExecution can classify whatever a crash left behind.
func (m *Launcher) Start(ctx context.Context, prepared PreparedRuntime) error {
	code, out, err := m.dock.Exec(ctx, string(prepared.RuntimeID), []string{supervisorPath, "start"})
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("start authorization exited %d: %s", code, out)
	}
	return nil
}

func (m *Launcher) prepare(ctx context.Context, spec Spec, rec ResourceRecorder) (string, error) {
	short := engine.ShortID(string(spec.LeaseID))
	name := func(role engine.Role) string { return "runpool-" + string(role) + "-" + short }
	labels := func(kind engine.ObjectKind, role engine.Role) map[string]string {
		return engine.Ownership{
			Instance: spec.InstanceID,
			Lease:    spec.LeaseID,
			Kind:     kind,
			Role:     role,
			Attempt:  spec.AttemptID,
			Target:   spec.TargetID,
			Tier:     spec.TierID,
		}.Labels()
	}
	resolve := func(kind engine.ObjectKind, objName string) func() (string, error) {
		return func() (string, error) {
			return m.dock.OwnedIDByName(ctx, kind, objName, spec.InstanceID, spec.LeaseID)
		}
	}

	// Sandboxed, the capsule's only network is internal with Engine 28's
	// isolated gateway mode: the bridge holds no host address, so the
	// gateway container is the single path out. Unsandboxed (the
	// explicit unsafe-open-egress profile) it is a plain bridge.
	sandboxed := spec.Sandbox != nil

	// One envelope for the lease, split between the capsule and — when
	// there is one — its gateway, and placed under one parent cgroup.
	cgroupParent := LeaseCgroupParent(spec.CgroupDriver, string(spec.LeaseID))
	capsuleShare := SplitEnvelope(spec.Resources, sandboxed)
	netID, err := m.create(ctx, rec, engine.KindNetwork, engine.RoleCapsuleNetwork, name(engine.RoleCapsuleNetwork),
		func() (string, error) {
			return m.dock.CreateNetwork(ctx, engine.NetworkSpec{
				Name:     name(engine.RoleCapsuleNetwork),
				Internal: sandboxed,
				Isolated: sandboxed,
				Labels:   labels(engine.KindNetwork, engine.RoleCapsuleNetwork),
			})
		}, resolve(engine.KindNetwork, name(engine.RoleCapsuleNetwork)))
	if err != nil {
		return "", err
	}

	var gatewayIP netip.Addr
	var internalSubnet string
	if sandboxed {
		if gatewayIP, internalSubnet, err = m.prepareGateway(ctx, spec, rec, netID, name, labels, resolve); err != nil {
			return "", err
		}
	}

	dindData, err := m.create(ctx, rec, engine.KindVolume, engine.RoleDindData, name(engine.RoleDindData),
		func() (string, error) {
			return m.dock.CreateVolume(ctx, name(engine.RoleDindData), labels(engine.KindVolume, engine.RoleDindData))
		}, resolve(engine.KindVolume, name(engine.RoleDindData)))
	if err != nil {
		return "", err
	}

	mounts := []engine.Mount{{Volume: dindData, Target: dindDataDir}}
	if !spec.Cache.empty() {
		mounts = append(mounts, engine.Mount{Volume: spec.Cache.Volume, Target: CachePath})
	}

	capsuleSpec := engine.ContainerSpec{
		Name:       name(engine.RoleCapsule),
		Image:      spec.CapsuleImage,
		Labels:     labels(engine.KindContainer, engine.RoleCapsule),
		Privileged: true,
		Network:    netID,
		Mounts:     mounts,
		// The control surface and delivered JIT bundle live on tmpfs and
		// die with the container.
		Tmpfs: map[string]string{controlDir: "rw,size=1m,mode=0755"},
		// The capsule's share of the tier envelope, applied exactly
		// once: this cgroup is the aggregate over runner, daemon and
		// every inner container the job launches. When a gateway
		// exists it holds the rest of the same envelope, so the two
		// together are the tier and never more.
		MemoryBytes:     capsuleShare.MemoryBytes,
		MemorySwapBytes: capsuleShare.MemorySwapBytes,
		NanoCPUs:        capsuleShare.NanoCPUs,
		PIDsLimit:       capsuleShare.PIDsLimit,
		CgroupParent:    cgroupParent,
	}
	if sandboxed {
		// The capsule resolves and reaches the network only through its
		// gateway. Both are pinned at creation: the resolver because
		// the runner must not consult the daemon's default, and the
		// proxy because it is the only egress that exists — the host
		// drops anything the capsule addresses beyond its own bridge.
		// The environment is inherited by the runner, by the job, and
		// by the inner daemon the supervisor starts, so image pulls
		// take the same path as everything else.
		capsuleSpec.DNS = []netip.Addr{gatewayIP}
		proxy := fmt.Sprintf("http://%s:%d", gatewayIP, GatewayProxyPort)
		noProxy := strings.Join([]string{"localhost", "127.0.0.1", innerDockerCIDR, internalSubnet}, ",")
		capsuleSpec.Env = append(capsuleSpec.Env,
			"HTTP_PROXY="+proxy, "http_proxy="+proxy,
			"HTTPS_PROXY="+proxy, "https_proxy="+proxy,
			"NO_PROXY="+noProxy, "no_proxy="+noProxy,
		)
	}
	outerID, err := m.create(ctx, rec, engine.KindContainer, engine.RoleCapsule, name(engine.RoleCapsule),
		func() (string, error) {
			return m.dock.CreateContainer(ctx, capsuleSpec)
		}, resolve(engine.KindContainer, name(engine.RoleCapsule)))
	if err != nil {
		return "", err
	}
	if err := m.dock.StartContainer(ctx, outerID); err != nil {
		return "", err
	}
	// The version first, then readiness. A capsule that does not speak
	// this protocol is refused in about a second rather than after the
	// readiness deadline, and its operator is told the image is the
	// problem instead of watching every job on the tier time out.
	if err := m.awaitProtocol(ctx, outerID); err != nil {
		return "", err
	}
	if err := m.awaitReady(ctx, outerID); err != nil {
		return "", err
	}
	// The JIT bundle travels over exec stdin onto the capsule's tmpfs, where
	// the supervisor holds it 0600 and runner-owned until Start consumes it.
	// The upstream runner's required --jitconfig argv boundary is inside the
	// capsule and is documented as accepted exposure in the threat model.
	code, out, err := m.dock.ExecWithInput(ctx, outerID,
		[]string{supervisorPath, "deliver"}, []byte(spec.JITConfig))
	if err != nil {
		return "", fmt.Errorf("deliver credential: %w", err)
	}
	if code != 0 {
		return "", fmt.Errorf("deliver credential: exited %d: %s", code, out)
	}
	return outerID, nil
}

// awaitProtocol refuses a capsule that does not speak this build's
// control protocol, before anything is handed to it and before readiness
// is waited on.
//
// The supervisor writes its version at boot, ahead of every state, so
// this reads a fact the capsule states about itself rather than inferring
// one from a verb that happened to work. Without it a capsule whose
// supervisor is older answers some verbs and not others, and the
// mismatch arrives as a job that failed rather than as an image that
// cannot be used — the difference between one operator reading one error
// and every job on that tier failing until someone correlates them.
//
// It polls because "before every state" is still after the container
// starts, and it does so under a bound of its own: this is the wait for
// a capsule to say what it is, not the wait for it to be ready, and
// conflating the two is what made a stated incompatibility cost a
// readiness deadline.
func (m *Launcher) awaitProtocol(ctx context.Context, outerID string) error {
	deadline := time.Now().Add(protocolTimeout)
	for {
		code, out, err := m.dock.Exec(ctx, outerID, []string{"cat", protocolFile})
		// Only a read that succeeded is a declaration to judge: a
		// capsule that has not written the file yet is not a capsule
		// that cannot.
		if err == nil && code == 0 {
			return protocolVerdict(code, out)
		}
		if err != nil {
			// A capsule that is no longer running will never write the
			// file, so waiting out the deadline for it spends thirty
			// seconds per attempt and then reports a read failure --
			// which is not the incompatibility the caller holds on, so
			// the tier retries the same broken image until its budget
			// runs out, under a reason that names none of this. The
			// fallback awaitState carries for the same shape.
			if state, serr := m.dock.ContainerStatus(ctx, outerID); serr == nil &&
				state.Status != engine.StatusRunning {
				return fmt.Errorf("%w: the capsule exited %d before declaring a control protocol%s",
					ErrIncompatibleImage, state.ExitCode, m.lastWords(ctx, outerID))
			}
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("read the capsule control protocol: %w", err)
			}
			// The last observation, judged: a capsule that never wrote
			// the file declares no protocol, which is what the verdict
			// says and what an operator has to act on.
			return protocolVerdict(code, out)
		}
		select {
		case <-time.After(readyPollInterval):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// lastWords is what a stopped container said, folded onto one line and
// prefixed for an error that would otherwise name only an exit code.
//
// It exists because the capsule that cannot start is usually the capsule
// that cannot say so. An image whose configured user is not root fails
// at boot on its first real write -- creating the daemon's socket
// directory under a root-owned /run, which returns before the protocol
// write into the control tmpfs is ever reached -- and then fails a
// second time writing the abort that would have explained it, onto that
// same tmpfs this launcher mounts root-owned, where the error is
// discarded too. What is left is a container that exited 79, from which
// the only account is "not a pair", sending an operator to re-check a
// digest that was correct. The reason was in the container log the whole
// time, and nothing read it.
//
// Best effort by construction: this runs on a path that is already
// failing, and a log that cannot be read must not replace the error that
// sent us here. Empty is the honest answer, and the error stands without
// it.
func (m *Launcher) lastWords(ctx context.Context, id string) string {
	tail, err := m.dock.TailLogs(ctx, id, logTailLines)
	if err != nil {
		return ""
	}
	var said []string
	for _, line := range strings.Split(strings.TrimSpace(tail), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			said = append(said, line)
		}
	}
	if len(said) == 0 {
		return ""
	}
	// Joined rather than kept as lines, because this lands in a
	// structured log field and a multi-line value there is one field
	// that reads as several.
	//
	// The log is as far as it goes, and that is worth being exact about:
	// what a held attempt stores is the review reason alone --
	// `capsule_incompatible` -- so `runpool attempts` is unchanged by
	// this. The operator who sees the hold still goes to the controller's
	// log to find out why, and the difference is that the answer is now
	// there.
	return "; it last said: " + strings.Join(said, " / ")
}

// ErrIncompatibleImage reports a capsule that does not speak this
// controller's control protocol. It is named so the caller can tell it
// from every other preparation failure: retrying is what a transient
// deserves, and this is a configured image that will fail the same way on
// every attempt until someone changes it.
var ErrIncompatibleImage = errors.New("the capsule image and this controller are not a pair")

// protocolVerdict is the decision the read produces, separated from the
// read so what can be wrong about it — the trimming, an exit code that is
// not zero, the message an operator acts on — is decidable without a
// daemon. The read itself is exercised by every capsule the live contract
// suite prepares, since a capsule that could not answer would not launch.
func protocolVerdict(code int, out string) error {
	got := strings.TrimSpace(out)
	switch {
	case code != 0:
		return fmt.Errorf("%w: it declares no control protocol at %s: exited %d: %s; "+
			"an operator's capsule image is built from the one this controller ships",
			ErrIncompatibleImage, protocolFile, code, got)
	case got != ProtocolVersion:
		return fmt.Errorf("%w: it speaks control protocol %q and this controller speaks %q",
			ErrIncompatibleImage, got, ProtocolVersion)
	}
	return nil
}

// prepareGateway builds the capsule's egress path before the capsule
// exists: gateway container on the internal bridge, second leg on the
// uplink, policy installed atomically, forwarder up — proven by the
// gateway reporting ready. Any failure here fails the launch closed;
// a capsule is never started with a half-made sandbox.
func (m *Launcher) prepareGateway(ctx context.Context, spec Spec, rec ResourceRecorder, netID string,
	name func(engine.Role) string, labels func(engine.ObjectKind, engine.Role) map[string]string,
	resolve func(engine.ObjectKind, string) func() (string, error)) (netip.Addr, string, error) {

	subnet, err := m.dock.NetworkSubnet(ctx, netID)
	if err != nil {
		return netip.Addr{}, "", fmt.Errorf("internal subnet: %w", err)
	}
	policy := egress.Policy{
		InternalSubnet: subnet,
		UplinkSubnet:   spec.Sandbox.UplinkSubnet,
		Allow:          spec.Sandbox.Allow,
		Deny:           spec.Sandbox.Deny,
	}
	if err := policy.Validate(); err != nil {
		return netip.Addr{}, "", fmt.Errorf("gateway policy: %w", err)
	}
	policyJSON, err := json.Marshal(policy)
	if err != nil {
		return netip.Addr{}, "", err
	}

	gwID, err := m.create(ctx, rec, engine.KindContainer, engine.RoleGateway, name(engine.RoleGateway),
		func() (string, error) {
			return m.dock.CreateContainer(ctx, engine.ContainerSpec{
				Name:       name(engine.RoleGateway),
				Image:      m.gatewayImage,
				Entrypoint: []string{supervisorPath, "gateway"},
				// The policy is configuration, not secret; the one
				// secret in the system never touches the gateway.
				Env:     []string{"RUNPOOL_GATEWAY_POLICY=" + string(policyJSON)},
				Labels:  labels(engine.KindContainer, engine.RoleGateway),
				Network: netID,
				// NET_ADMIN for the ruleset, and nothing else: not
				// privileged, no socket, no volumes, no credentials.
				CapAdd: []string{"NET_ADMIN"},
				Tmpfs:  map[string]string{controlDir: "rw,size=1m,mode=0755"},
				// The gateway's share of the lease's envelope, under
				// the lease's parent cgroup. Every connection a job
				// opens is work this container performs, so it lives
				// inside the budget rather than beside it.
				MemoryBytes:     GatewayEnvelope().MemoryBytes,
				MemorySwapBytes: GatewayEnvelope().MemorySwapBytes,
				NanoCPUs:        GatewayEnvelope().NanoCPUs,
				PIDsLimit:       GatewayEnvelope().PIDsLimit,
				CgroupParent:    LeaseCgroupParent(spec.CgroupDriver, string(spec.LeaseID)),
			})
		}, resolve(engine.KindContainer, name(engine.RoleGateway)))
	if err != nil {
		return netip.Addr{}, "", err
	}
	if err := m.dock.ConnectNetwork(ctx, spec.Sandbox.UplinkNetworkID, gwID); err != nil {
		return netip.Addr{}, "", fmt.Errorf("attach gateway to uplink: %w", err)
	}
	if err := m.dock.StartContainer(ctx, gwID); err != nil {
		return netip.Addr{}, "", err
	}
	if err := m.awaitState(ctx, gwID, protocol.StateReady); err != nil {
		return netip.Addr{}, "", fmt.Errorf("gateway: %w", err)
	}
	ip, _, err := m.dock.ContainerIPOn(ctx, gwID, netID)
	if err != nil {
		return netip.Addr{}, "", err
	}
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return netip.Addr{}, "", err
	}
	return addr, subnet, nil
}

// awaitReady polls the supervisor's state until the capsule reports
// waiting: daemon booted, readiness proven, runner deliberately not
// started. The supervisor writes that state only once its daemon
// answers, so reaching it is the proof — not the intention — that a job
// delivered here has something to run on.
func (m *Launcher) awaitReady(ctx context.Context, outerID string) error {
	return m.awaitState(ctx, outerID, protocol.StateWaiting)
}

// stateVerdict is what one observation of a supervisor-family container
// decides: the wanted state reached, a terminal state it can never leave,
// or keep polling. It is separated from the poll because the decision is
// where the cost is — a terminal state carries the reason the container
// failed, that reason dies with the container, and treating it as "not
// yet" spends the whole deadline and then reports a timeout instead.
func stateVerdict(want protocol.State, code int, out string, execErr error) (done bool, err error) {
	if execErr != nil || code != 0 {
		return false, nil
	}
	switch s := protocol.State(strings.TrimSpace(out)); {
	case s == want:
		return true, nil
	case protocol.Terminal(s):
		return true, fmt.Errorf("the container reported %s", s)
	default:
		return false, nil
	}
}

// awaitState polls a supervisor-family container until it reports the
// wanted state or one it can no longer leave.
//
// An exec into a container that is no longer running always fails, and a
// supervisor writes its terminal state and then exits — so that state is
// readable for an instant and a poll can miss it. Treated as "not yet",
// every such failure spends the whole deadline and reports a timeout
// naming nothing an operator can act on, which is the outcome
// stateVerdict exists to prevent for a container that stays up. Asking
// the daemon whether the container is still there is what covers the one
// that does not.
func (m *Launcher) awaitState(ctx context.Context, containerID string, want protocol.State) error {
	deadline := time.Now().Add(readyTimeout)
	for {
		code, out, err := m.dock.Exec(ctx, containerID, []string{supervisorPath, "state"})
		if done, verdict := stateVerdict(want, code, out, err); done {
			return verdict
		}
		if err != nil {
			// Best effort: a daemon that cannot answer this leaves the
			// poll exactly where it was, which is the deadline below.
			if state, serr := m.dock.ContainerStatus(ctx, containerID); serr == nil && state.Status != engine.StatusRunning {
				return fmt.Errorf("container %s exited %d before reaching %q%s",
					engine.ShortID(containerID), state.ExitCode, want, m.lastWords(ctx, containerID))
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("container %s did not reach %q within %s", engine.ShortID(containerID), want, readyTimeout)
		}
		select {
		case <-time.After(readyPollInterval):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
