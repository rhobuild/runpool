package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/rhobuild/runpool/internal/capsule"
	"github.com/rhobuild/runpool/internal/capsule/protocol"
	"github.com/rhobuild/runpool/internal/config"
	"github.com/rhobuild/runpool/internal/egress"
	"github.com/rhobuild/runpool/internal/platform/docker"

	"github.com/rhobuild/runpool/internal/assignment"
)

// sandboxNetworks is what computing an egress policy needs from the
// daemon: the instance uplink, its subnet, every subnet the daemon knows
// and a probe container to look at the host with.
type sandboxNetworks interface {
	EnsureOwnedNetwork(ctx context.Context, spec docker.NetworkSpec) (string, error)
	NetworkSubnet(ctx context.Context, id string) (string, error)
	AllNetworkSubnets(ctx context.Context) ([]string, error)
	RunTask(ctx context.Context, spec docker.ContainerSpec) (int64, string, error)
}

// sandboxGateways is what installing one needs: find the gateways, hand
// each the new sets, and remove the ones that will not take them.
type sandboxGateways interface {
	ListOwnedContainers(ctx context.Context, instanceID assignment.InstanceID) ([]docker.OwnedContainer, error)
	Exec(ctx context.Context, id string, cmd []string) (int, string, error)
	ExecWithInput(ctx context.Context, id string, cmd []string, input []byte) (int, string, error)
	RemoveContainer(ctx context.Context, id string) error
}

// sandboxDaemon is both halves, which one *docker.Client satisfies. They
// are named apart because they are two jobs, and because a fake for one
// can embed the other and leave it unimplemented.
type sandboxDaemon interface {
	sandboxNetworks
	sandboxGateways
}

// sandboxState owns the current restricted-network snapshot. Capsule
// launches receive deep copies, so a rediscovery cannot mutate policy while
// a concurrent launch is serializing it. refreshing also serializes the
// external discovery/reload operation: policy changes must be applied in the
// same order in which they are observed.
//
// It is a channel rather than a mutex because waiting for it has to be
// abandonable. What that costs is ownership: the token is bare, so a
// drain anywhere other than the send's own defer releases it whoever
// holds it, and two refreshes then run at once with nothing to say so.
// refresh is the only place that takes it. A launch holds it for as long as a discovery and a
// gateway fan-out take, on a context that deliberately outlives the
// serve loop so cleanup can still run; a mutex would park the shutdown
// pass on that launch with no way to give up, and the wait for the serve
// loops is unbounded by design. Past the grace period that is a SIGKILL,
// which leaves every message session open for the next start to wait out
// as a conflict.
type sandboxState struct {
	mu         sync.RWMutex
	refreshing chan struct{}
	current    *capsule.Sandbox
}

func newSandboxState(initial *capsule.Sandbox) *sandboxState {
	return &sandboxState{current: cloneSandbox(initial), refreshing: make(chan struct{}, 1)}
}

func (s *sandboxState) snapshot() *capsule.Sandbox {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneSandbox(s.current)
}

func (s *sandboxState) replace(next *capsule.Sandbox) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.current = cloneSandbox(next)
}

func cloneSandbox(in *capsule.Sandbox) *capsule.Sandbox {
	if in == nil {
		return nil
	}
	out := *in
	out.Allow = append([]string(nil), in.Allow...)
	out.Deny = append([]string(nil), in.Deny...)
	return &out
}

// networkSandbox owns the restricted profile's egress policy: it
// computes the deny set from the host, keeps the snapshot capsule
// launches are cut from, and installs changes into the running gateways.
//
// A nil *networkSandbox is the unsafe-open-egress profile — there is no
// policy to maintain — and every method here answers for that, so the
// serving paths ask without first asking whether there is anything to
// ask.
type networkSandbox struct {
	log        *slog.Logger
	daemon     sandboxDaemon
	instanceID assignment.InstanceID
	probeImage string
	// allow and denies are the operator's own two lists, read once. They
	// extend the discovered set rather than replacing anything in it, and
	// they are kept so a rediscovery rebuilds the policy with them still
	// in place.
	allow  []string
	denies []string

	state *sandboxState
}

// newNetworkSandbox assembles the initial policy and fails closed. Any
// gap in the deny set is a hole in every capsule's egress, so serve does
// not start without a complete one.
func newNetworkSandbox(ctx context.Context, daemon sandboxDaemon,
	instanceID assignment.InstanceID, probeImage string,
	cfg *config.Config, log *slog.Logger) (*networkSandbox, error) {
	n := &networkSandbox{
		log: log, daemon: daemon, instanceID: assignment.InstanceID(instanceID), probeImage: probeImage,
	}
	for _, c := range cfg.Network.AllowPrivateCIDRs {
		n.allow = append(n.allow, c.String())
	}
	for _, c := range cfg.Network.DenyCIDRs {
		n.denies = append(n.denies, c.String())
	}
	initial, err := n.build(ctx, sandboxFirstBuildBudget)
	if err != nil {
		return nil, err
	}
	n.state = newSandboxState(initial)
	log.Info("network sandbox ready", "uplink_subnet", initial.UplinkSubnet,
		"deny", len(initial.Deny), "allow", len(initial.Allow))
	return n, nil
}

// build assembles the sandbox input: the instance uplink (created or
// adopted under proven ownership) and the deny-set snapshot — host
// interface networks discovered through a short-lived host-namespace
// probe, every Docker subnet the daemon knows, the baseline ranges, and
// the uplink itself.
func (n *networkSandbox) build(ctx context.Context, budget time.Duration) (*capsule.Sandbox, error) {
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	uplinkID, err := n.daemon.EnsureOwnedNetwork(ctx, docker.NetworkSpec{
		Name:   "runpool-uplink-" + string(n.instanceID)[:8],
		Labels: docker.Ownership{Instance: n.instanceID, Role: capsule.RoleUplink}.Labels(),
	})
	if err != nil {
		return nil, fmt.Errorf("uplink network: %w", err)
	}
	uplinkSubnet, err := n.daemon.NetworkSubnet(ctx, uplinkID)
	if err != nil {
		return nil, fmt.Errorf("uplink subnet: %w", err)
	}
	hostCIDRs, err := n.discoverHostCIDRs(ctx)
	if err != nil {
		return nil, fmt.Errorf("host discovery: %w", err)
	}
	dockerSubnets, err := n.daemon.AllNetworkSubnets(ctx)
	if err != nil {
		return nil, fmt.Errorf("docker subnets: %w", err)
	}
	return &capsule.Sandbox{
		UplinkNetworkID: uplinkID,
		UplinkSubnet:    uplinkSubnet,
		Allow:           n.allow,
		Deny:            egress.BuildDeny(uplinkSubnet, hostCIDRs, append(dockerSubnets, n.denies...)),
	}, nil
}

// rediscoverInterval is how often the deny set is recomputed against
// the host. Networks appear when someone starts an unrelated stack;
// the cost of noticing late is a hole that stays open until then, and
// the cost of noticing often is one probe container.
const rediscoverInterval = 5 * time.Minute

// The two budgets on discovering the environment. It is one budget over
// the whole of build rather than one per step, because what it protects
// is a caller that cannot give up rather than any particular call, and
// which step hung does not change that.
//
// They differ because their callers do. The first build runs from
// newNetworkSandbox, before the state exists and before any binding does:
// nothing holds the refresh slot, nothing waits for it, and the probe
// image may still have to be pulled — it is the capsule image, which is
// hundreds of megabytes. Failing that for being slow is a controller
// that will not start on a cold host with an ordinary link.
//
// Every later build runs from refresh, holding the slot every launch
// waits for — and a launch waits without a bound on purpose, since one
// that skipped the policy would run unconfined — so an unbounded step
// there is a host on which nothing new can start. By then the image
// is present, so what remains is creating a network, inspecting it,
// running a small container to completion and listing subnets. The bound
// stays under rediscoverInterval so a build that hangs cannot leave two
// passes overlapping, which TestDiscoveringTheEnvironmentIsBounded
// requires.
//
// What this does not cover: an image removed from under a running
// controller makes a later build pull again, on the tighter budget, and
// a refresh that fails closes every gateway. That host has lost the
// image its capsules run from and is going to fail launches regardless.
const (
	sandboxFirstBuildBudget = 10 * time.Minute
	sandboxRefreshBudget    = 2 * time.Minute
)

// PolicyChange classifies what a rediscovered deny set does to the one
// in force. The distinction decides what a failure to install it costs.
type PolicyChange int

const (
	// PolicyUnchanged: nothing to install.
	PolicyUnchanged PolicyChange = iota
	// PolicyRestriction: the new set denies everything the old one did
	// and more. Until it is installed, a capsule can reach an address
	// that should now be denied, so failing to install it is a security
	// failure, not a delay.
	PolicyRestriction
	// PolicyRelaxation: the new set denies strictly less. Failing to
	// install it costs a job some reachability and nothing else, so
	// established work is preserved.
	PolicyRelaxation
	// PolicyMixed: it both adds and removes denies. It carries a
	// restriction, so it is treated as one.
	PolicyMixed
)

func (c PolicyChange) String() string {
	switch c {
	case PolicyUnchanged:
		return "unchanged"
	case PolicyRestriction:
		return "restriction"
	case PolicyRelaxation:
		return "relaxation"
	case PolicyMixed:
		return "mixed"
	}
	return "unknown"
}

// restricts reports whether the change adds any deny — the property
// that makes a failed install unsafe rather than merely stale.
func (c PolicyChange) restricts() bool { return c == PolicyRestriction || c == PolicyMixed }

// ClassifyPolicy compares two deny sets as sets of prefixes.
//
// The comparison is textual on purpose: the deny sets are built by one
// function from discovered facts, so a range that is still denied is
// still denied by the same prefix string. Treating "10.0.0.0/8 became
// 10.0.0.0/9" as anything other than a change would require a
// containment analysis whose mistakes would all point the wrong way.
func ClassifyPolicy(inForce, next []string) PolicyChange {
	had := make(map[string]bool, len(inForce))
	for _, c := range inForce {
		had[c] = true
	}
	has := make(map[string]bool, len(next))
	for _, c := range next {
		has[c] = true
	}
	var added, removed bool
	for c := range has {
		if !had[c] {
			added = true
		}
	}
	for c := range had {
		if !has[c] {
			removed = true
		}
	}
	switch {
	case added && removed:
		return PolicyMixed
	case added:
		return PolicyRestriction
	case removed:
		return PolicyRelaxation
	default:
		return PolicyUnchanged
	}
}

// watch repeats discovery and installs what it finds.
//
// The old policy is not a safe fallback. It is merely older: if a host
// interface or a Docker network appeared since it was computed, the
// capsules running under it can reach an address that should now be
// denied. So a failure is classified by what it leaves reachable —
// a restriction that cannot be installed closes the affected gateway to
// everything, and discovery that cannot be trusted at all does the same
// for every gateway, because an undiscovered subnet is indistinguishable
// from one that was never there.
func (n *networkSandbox) watch(ctx context.Context) {
	if n == nil {
		return // unsafe-open-egress: there is no policy to maintain
	}
	ticker := time.NewTicker(rediscoverInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		n.rediscover(ctx)
	}
}

// forLaunch re-proves the instance uplink and the complete deny set
// immediately before a capsule is admitted. This makes an idle uplink removed
// by a platform cleanup recoverable, and it prevents a shared daemon's newly
// created networks from remaining reachable until the periodic refresh.
func (n *networkSandbox) forLaunch(ctx context.Context) (*capsule.Sandbox, error) {
	if n == nil {
		return nil, nil
	}
	if err := n.refresh(ctx); err != nil {
		return nil, err
	}
	return n.state.snapshot(), nil
}

func (n *networkSandbox) refresh(ctx context.Context) error {
	select {
	case n.state.refreshing <- struct{}{}:
	case <-ctx.Done():
		// Whoever holds it is a launch, running on a context of its own,
		// and it will finish in its own time. This caller's context is
		// what says the wait is over.
		return ctx.Err()
	}
	defer func() { <-n.state.refreshing }()

	next, err := n.build(ctx, sandboxRefreshBudget)
	if err != nil {
		return err
	}
	return n.applyPolicy(ctx, next)
}

// rediscover is one pass, separated so a test can drive it directly.
func (n *networkSandbox) rediscover(ctx context.Context) {
	err := n.refresh(ctx)
	if err == nil {
		return
	}
	if ctx.Err() != nil {
		// The pass is being stopped, not failing. Closing every gateway
		// needs the same context to reach the daemon, so the attempt
		// below could not do anything even if it were right to try -- and
		// what it would leave behind is an error line announcing that all
		// egress was cut, on an ordinary shutdown, for a thing that
		// provably did not happen.
		n.log.Info("sandbox rediscovery stopped", "reason", ctx.Err())
		return
	}

	// Discovery failed. The environment may have grown a network this
	// policy does not deny, and there is no way to tell — so every
	// gateway closes rather than relaying under a set that cannot be
	// shown to be current.
	n.log.Error("sandbox rediscovery failed; closing every gateway to all egress", "error", err)
	if err := n.closeGateways(ctx); err != nil {
		n.log.Error("could not close every gateway after a failed rediscovery", "error", err)
	}
}

// applyPolicy installs a rediscovered set, and decides what a failure
// costs from what the change was.
//
// It reports whether the install could be attempted at all, because the
// caller's fail-closed paths turn on exactly that: a discovery pass that
// cannot say what is installed anywhere closes every gateway, and a
// launch under an unprovable policy is refused.
func (n *networkSandbox) applyPolicy(ctx context.Context, next *capsule.Sandbox) error {
	previous := n.state.snapshot()
	change := ClassifyPolicy(previous.Deny, next.Deny)
	uplinkChanged := previous.UplinkNetworkID != next.UplinkNetworkID
	if change == PolicyUnchanged && !uplinkChanged {
		return nil
	}
	n.log.Info("egress policy changed", "change", change.String(),
		"was", len(previous.Deny), "now", len(next.Deny), "uplink_recreated", uplinkChanged)

	failed, err := n.reloadGateways(ctx, next.Allow, next.Deny)
	if err != nil {
		// Nothing can be said about what is installed anywhere, so the
		// snapshot stays where it was and the next pass sees this change
		// again. Recorded first, that pass would compare the new set
		// against itself, report it unchanged, and a restriction that
		// landed nowhere would never be attempted again.
		return fmt.Errorf("enumerate gateways: %w", err)
	}
	// Recorded only now: the set has reached every gateway that could be
	// named, so this is what is in force rather than what was intended.
	// The refresh slot is held across both, so no launch can observe the
	// window between them.
	n.state.replace(next)
	if len(failed) == 0 {
		return nil
	}
	if !change.restricts() {
		// A relaxation that did not land leaves the capsule with the
		// stricter policy it started under. Its work continues.
		n.log.Warn("some gateways kept the previous, stricter policy",
			"gateways", len(failed), "change", change.String())
		return nil
	}
	// A restriction that did not land leaves a capsule able to reach
	// something the policy now denies. Close those gateways.
	n.log.Error("a restriction could not be installed; closing the affected gateways",
		"gateways", len(failed))
	eachGateway(failed, func(id string) {
		if err := n.closeGateway(ctx, id); err != nil {
			n.log.Error("a gateway could not be closed and may still relay a denied address",
				"container", id, "error", err)
		}
	})
	return nil
}

// gatewayFanout is how many gateway control commands run at once.
//
// A refresh pass holds the refresh slot and every launch waits for it, so
// the pass's duration is a launch's worst-case wait — and walking the
// gateways one at a time made that duration the number of gateways times
// the exec bound. There is one gateway per running capsule, so it grew
// with exactly the parallelism the host was configured for: at
// thirty-two in flight, sixteen minutes with every launch stopped, for
// one policy change. A launch waiting on its own context can give that
// wait up; a launch that still needs the policy cannot, which is why the
// duration is bounded here rather than only survivable.
//
// Bounded rather than unbounded. These are execs into containers on a
// single daemon, and a pass that opens one per capsule trades a slow
// refresh for a slow daemon, which every other caller then waits on too.
const gatewayFanout = 8

// eachGateway runs fn over every item concurrently, at most
// gatewayFanout at a time, and returns once all of them have finished.
// fn is called from several goroutines, so what it touches must be safe
// for that.
func eachGateway[T any](items []T, fn func(T)) {
	var wg sync.WaitGroup
	slots := make(chan struct{}, gatewayFanout)
	for _, item := range items {
		wg.Add(1)
		slots <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			fn(item)
		}()
	}
	wg.Wait()
}

// reloadGateways hands the new sets to every running gateway this
// instance owns. A gateway that cannot be reloaded is reported, not
// worked around. It returns the ids of the gateways that did not take
// the new policy, so the caller can decide what that costs from what
// the change was.
//
// Those ids come back in whatever order the daemon answered, because the
// gateways are reached concurrently. No caller may depend on it. Sorting
// here would be a second mechanism for a property nothing reads — the
// one caller takes a length and iterates order-blind, and a test that
// compares the set sorts what it compares.
func (n *networkSandbox) reloadGateways(ctx context.Context, allow, deny []string) (failed []string, err error) {
	containers, err := n.ownedContainers(ctx)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(egress.Policy{Allow: allow, Deny: deny})
	if err != nil {
		return nil, err
	}
	var mu sync.Mutex
	eachGateway(containers, func(c docker.OwnedContainer) {
		if c.Role != capsule.RoleGateway || !c.Running {
			return
		}
		code, out, err := n.execGateway(ctx, func(ctx context.Context) (int, string, error) {
			return n.daemon.ExecWithInput(ctx, c.ID,
				[]string{capsule.SupervisorPath, "gateway-reload"}, payload)
		})
		if err != nil || code != 0 {
			mu.Lock()
			failed = append(failed, c.ID)
			mu.Unlock()
			n.log.Error("gateway policy reload failed", "container", c.Name, "exit", code, "error", err, "output", out)
			return
		}
		n.log.Info("gateway policy reloaded", "container", c.Name)
	})
	return failed, nil
}

// gatewayControlTimeout bounds one gateway control operation — an exec
// into it, or its removal — so a refresh pass costs a known number of
// these rather than an unknown amount. The pass holds the refresh slot,
// a launch waits for it without a bound of its own, and the context this
// runs on carries no deadline: anything unbounded under it is unbounded
// for every launch on the host.
const gatewayControlTimeout = 30 * time.Second

// ownedContainers asks the daemon what this instance owns, under a bound.
//
// Both callers run holding the refresh slot, and enumerating is the
// first thing each does — so a daemon that accepts the request and
// answers nothing would hold that slot before any gateway had been
// reached, with every launch on the host waiting for it and no deadline
// on the context to end the wait. It is the same reason the exec and the
// removal below are bounded, and leaving it out left the pass unbounded
// at its very first step.
func (n *networkSandbox) ownedContainers(ctx context.Context) ([]docker.OwnedContainer, error) {
	ctx, cancel := context.WithTimeout(ctx, gatewayControlTimeout)
	defer cancel()
	return n.daemon.ListOwnedContainers(ctx, n.instanceID)
}

// execGateway runs one gateway control command under its own bound.
func (n *networkSandbox) execGateway(ctx context.Context,
	call func(context.Context) (int, string, error)) (int, string, error) {

	ctx, cancel := context.WithTimeout(ctx, gatewayControlTimeout)
	defer cancel()
	return call(ctx)
}

// closeGateway revokes a live gateway's egress: every destination
// denied, in the kernel ruleset and in the relay's own check.
//
// Closing the policy is not enough on its own — an established tunnel
// is already through the check — so the gateway is stopped afterwards,
// which takes its connections with it. A capsule whose gateway is gone
// has no egress at all, which is the state this is trying to reach.
func (n *networkSandbox) closeGateway(ctx context.Context, containerID string) error {
	code, out, err := n.execGateway(ctx, func(ctx context.Context) (int, string, error) {
		return n.daemon.Exec(ctx, containerID,
			[]string{capsule.SupervisorPath, protocol.GatewayDenyAllCommand})
	})
	if err != nil || code != 0 {
		// The policy could not be closed from inside; removing the
		// container is the remaining way to stop it relaying.
		n.log.Error("gateway deny-all failed; removing the gateway",
			"container", containerID, "exit", code, "error", err, "output", out)
	}
	// Bounded like the exec above it, and for the same reason. This runs
	// holding the refresh slot, which every launch waits for: a daemon
	// that accepts the removal and then answers nothing would hold it for
	// the life of the process, on a context with no deadline to end the
	// wait. That is the failure the fan-out above exists to bound, and
	// leaving it here left it in the same function.
	//
	// A removal that cannot be confirmed is reported. The gateway is
	// still there, so the next pass finds it and closes it again.
	ctx, cancel := context.WithTimeout(ctx, gatewayControlTimeout)
	defer cancel()
	return n.daemon.RemoveContainer(ctx, containerID)
}

// closeGateways closes every live gateway this instance owns.
func (n *networkSandbox) closeGateways(ctx context.Context) error {
	containers, err := n.ownedContainers(ctx)
	if err != nil {
		return err
	}
	var (
		mu       sync.Mutex
		failures int
	)
	eachGateway(containers, func(c docker.OwnedContainer) {
		if c.Role != capsule.RoleGateway || !c.Running {
			return
		}
		if err := n.closeGateway(ctx, c.ID); err != nil {
			mu.Lock()
			failures++
			mu.Unlock()
			n.log.Error("a gateway could not be closed", "container", c.Name, "error", err)
		}
	})
	if failures > 0 {
		return fmt.Errorf("%d gateway(s) could not be closed", failures)
	}
	return nil
}

// discoverHostCIDRs enumerates the host's global IPv4 networks with a
// short-lived probe in the host network namespace: read-only, no
// capabilities, no socket, no volumes — it looks and reports. An empty
// answer is a failure, not a finding: a host has interfaces, and a
// deny set built from a blind probe would allow what it cannot see.
func (n *networkSandbox) discoverHostCIDRs(ctx context.Context) ([]string, error) {
	nonce := make([]byte, 4)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	code, out, err := n.daemon.RunTask(ctx, docker.ContainerSpec{
		Name:        "runpool-" + string(n.instanceID)[:8] + "-hostnet-probe-" + hex.EncodeToString(nonce),
		Image:       n.probeImage,
		Entrypoint:  []string{"/bin/sh", "-c"},
		Cmd:         []string{"ip -o -4 addr show scope global"},
		NetworkMode: "host",
		Labels:      docker.Ownership{Instance: n.instanceID, Role: "probe"}.Labels(),
	})
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("host probe exited %d: %s", code, out)
	}
	cidrs, err := parseHostCIDRs(out)
	if err != nil {
		return nil, err
	}
	if len(cidrs) == 0 {
		return nil, fmt.Errorf("host probe saw no global IPv4 networks; refusing a blind deny set")
	}
	return cidrs, nil
}

// parseHostCIDRs reads `ip -o -4 addr show` one-line output: the field
// after "inet" is address/prefix, masked here to the network.
func parseHostCIDRs(out string) ([]string, error) {
	seen := map[string]bool{}
	var cidrs []string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		for i, f := range fields {
			if f != "inet" || i+1 >= len(fields) {
				continue
			}
			prefix, err := netip.ParsePrefix(fields[i+1])
			if err != nil {
				return nil, fmt.Errorf("host probe line %q: %w", line, err)
			}
			network := prefix.Masked().String()
			if !seen[network] {
				seen[network] = true
				cidrs = append(cidrs, network)
			}
		}
	}
	return cidrs, nil
}
