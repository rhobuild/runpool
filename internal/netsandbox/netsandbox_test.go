package netsandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rhobuild/runpool/internal/capsule"
	"github.com/rhobuild/runpool/internal/capsule/protocol"
	"github.com/rhobuild/runpool/internal/config"
	"github.com/rhobuild/runpool/internal/engine"

	"github.com/rhobuild/runpool/internal/assignment"
)

func TestSandboxStateSnapshotsAreIndependent(t *testing.T) {
	original := &capsule.Sandbox{
		UplinkNetworkID: "uplink-1",
		UplinkSubnet:    "172.20.0.0/16",
		Allow:           []string{"203.0.113.0/24"},
		Deny:            []string{"10.0.0.0/8"},
	}
	state := newSandboxState(original)

	original.Deny[0] = "changed-outside"
	first := state.snapshot()
	first.Allow[0] = "changed-snapshot"
	if got := state.snapshot(); !reflect.DeepEqual(got.Allow, []string{"203.0.113.0/24"}) ||
		!reflect.DeepEqual(got.Deny, []string{"10.0.0.0/8"}) {
		t.Fatalf("sandbox state was aliased across owners: %+v", got)
	}

	state.replace(&capsule.Sandbox{UplinkNetworkID: "uplink-2", Deny: []string{"192.168.0.0/16"}})
	if got := state.snapshot(); got.UplinkNetworkID != "uplink-2" || !reflect.DeepEqual(got.Deny, []string{"192.168.0.0/16"}) {
		t.Fatalf("replacement snapshot = %+v", got)
	}
}

// TestClassifyPolicy is what decides whether a failed reload is a delay
// or a security failure. A change that adds a deny leaves capsules able
// to reach something that should now be blocked until it lands; one
// that only removes denies costs reachability and nothing else.
func TestClassifyPolicy(t *testing.T) {
	base := []string{"10.0.0.0/8", "192.168.0.0/16"}

	cases := []struct {
		name      string
		inForce   []string
		next      []string
		want      PolicyChange
		restricts bool
	}{
		{"identical", base, []string{"192.168.0.0/16", "10.0.0.0/8"}, PolicyUnchanged, false},
		{"a new docker network appeared", base, append(append([]string{}, base...), "172.30.0.0/16"), PolicyRestriction, true},
		{"a host network went away", base, []string{"10.0.0.0/8"}, PolicyRelaxation, false},
		{"one replaced by another", base, []string{"10.0.0.0/8", "172.30.0.0/16"}, PolicyMixed, true},
		{"everything went away", base, nil, PolicyRelaxation, false},
		{"from nothing", nil, base, PolicyRestriction, true},
	}
	for _, tc := range cases {
		got := ClassifyPolicy(tc.inForce, tc.next)
		if got != tc.want {
			t.Errorf("%s: classified %s; want %s", tc.name, got, tc.want)
		}
		if got.restricts() != tc.restricts {
			t.Errorf("%s: restricts() = %v; want %v", tc.name, got.restricts(), tc.restricts)
		}
	}
}

// TestPolicyChangeNamesItself keeps the log readable: an operator
// reading "change=restriction" learns why their capsule was closed.
func TestPolicyChangeNamesItself(t *testing.T) {
	for _, c := range []PolicyChange{PolicyUnchanged, PolicyRestriction, PolicyRelaxation, PolicyMixed} {
		if c.String() == "unknown" {
			t.Errorf("PolicyChange(%d) has no name", c)
		}
	}
}

// fakeDaemon answers for both halves of the daemon the sandbox
// uses. Its point is the states that decide the design: a probe that sees
// nothing, and a gateway that refuses the policy it is handed.
type fakeDaemon struct {
	uplinkID     string
	uplinkSubnet string
	subnets      []string
	probeOut     string
	probeErr     error

	containers []engine.OwnedContainer
	// listErr is the daemon refusing to say which gateways exist, which
	// is the one failure that makes the whole install unaccountable.
	listErr error
	// refuse names the containers whose exec fails, which is how a
	// gateway that will not take a new policy is expressed.
	refuse map[string]bool
	// removeErr names the containers the daemon will not remove, which is
	// how a gateway that can neither be reloaded nor closed is expressed.
	removeErr map[string]error
	// onList runs inside the enumeration, after the answer to this call
	// has been taken. Appending there is how a gateway appears between a
	// pass listing them and that pass finishing, which is the moment a
	// launch creates one.
	onList func(*fakeDaemon)
	// delay is how long each gateway control command takes, which is
	// what makes a serial pass distinguishable from a concurrent one.
	delay time.Duration

	// The pass fans out over gateways now, so what it records is written
	// from several goroutines and in no fixed order. The mutex is the
	// fake's own; the accessors below sort, so a test asserts what was
	// touched rather than the order the daemon answered in.
	mu       sync.Mutex
	inFlight int
	peak     int
	// removeDeadline is the bound a container removal ran under, which
	// is where an unbounded one would otherwise be invisible.
	removeDeadline time.Time
	removeBounded  bool
	// buildDeadline is the bound the environment discovery ran under.
	// The probe waits on a process rather than on a request, so an
	// unbounded one there is unbounded for every launch on the host.
	buildDeadline time.Time
	buildBounded  bool
	// listDeadline is the bound the gateway enumeration ran under. It is
	// the first thing a refresh pass does, so an unbounded one there
	// holds the lock before any gateway has been reached.
	listDeadline time.Time
	listBounded  bool
	// lists counts enumerations, which is what separates a confirmation
	// that had nothing to do from one that reached the daemon.
	lists    int
	reloaded []string
	removed  []string
	// events is what the daemon was asked to do, in order, as
	// "<verb>:<container>". Two sorted sets cannot say that the
	// revocation came before the removal, and per-gateway order is
	// deterministic even under the fan-out, so one ordered log is both
	// smaller and stronger.
	events []string
}

// serve models one gateway control command occupying the daemon for as
// long as it takes, which is what the concurrency counters below see.
func (f *fakeDaemon) serve() {
	f.enter()
	defer f.leave()
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
}

func (f *fakeDaemon) note(into *[]string, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	*into = append(*into, id)
}

// enter and leave track how many control commands the daemon is serving
// at once. That is what a refresh's cost turns on, and unlike wall-clock
// time it does not change with how loaded the machine is.
func (f *fakeDaemon) enter() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inFlight++
	if f.inFlight > f.peak {
		f.peak = f.inFlight
	}
}

func (f *fakeDaemon) leave() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inFlight--
}

func (f *fakeDaemon) peakInFlight() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.peak
}

func (f *fakeDaemon) sorted(from *[]string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := slices.Clone(*from)
	slices.Sort(out)
	return out
}

func (f *fakeDaemon) reloads() []string { return f.sorted(&f.reloaded) }

// eventsFor is the ordered record for one container.
func (f *fakeDaemon) eventsFor(id string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, e := range f.events {
		if strings.HasSuffix(e, ":"+id) {
			out = append(out, e)
		}
	}
	return out
}
func (f *fakeDaemon) removes() []string { return f.sorted(&f.removed) }

func (f *fakeDaemon) EnsureOwnedNetwork(context.Context, engine.NetworkSpec) (string, error) {
	return f.uplinkID, nil
}
func (f *fakeDaemon) NetworkSubnet(context.Context, string) (string, error) {
	return f.uplinkSubnet, nil
}
func (f *fakeDaemon) AllNetworkSubnets(context.Context) ([]string, error) {
	return f.subnets, nil
}
func (f *fakeDaemon) RunTask(ctx context.Context, _ engine.ContainerSpec) (int64, string, error) {
	f.mu.Lock()
	f.buildDeadline, f.buildBounded = ctx.Deadline()
	f.mu.Unlock()
	return 0, f.probeOut, f.probeErr
}
func (f *fakeDaemon) ListOwnedContainers(ctx context.Context, _ assignment.InstanceID) ([]engine.OwnedContainer, error) {
	f.mu.Lock()
	f.listDeadline, f.listBounded = ctx.Deadline()
	f.lists++
	answer := slices.Clone(f.containers)
	if f.onList != nil {
		f.onList(f)
	}
	f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	return answer, nil
}
func (f *fakeDaemon) Exec(_ context.Context, id string, cmd []string) (int, string, error) {
	f.serve()
	// The command, not merely that some exec happened: closing a gateway
	// and reloading one are both execs, and only one of them revokes.
	f.note(&f.events, cmd[len(cmd)-1]+":"+id)
	if f.refuse[id] {
		return 1, "refused", nil
	}
	return 0, "", nil
}
func (f *fakeDaemon) ExecWithInput(_ context.Context, id string, _ []string, _ []byte) (int, string, error) {
	f.serve()
	if f.refuse[id] {
		return 1, "refused", nil
	}
	f.note(&f.reloaded, id)
	return 0, "", nil
}
func (f *fakeDaemon) RemoveContainer(ctx context.Context, id string) error {
	f.mu.Lock()
	f.removeDeadline, f.removeBounded = ctx.Deadline()
	f.mu.Unlock()
	f.serve()
	f.note(&f.events, "remove:"+id)
	if err := f.removeErr[id]; err != nil {
		return err
	}
	f.note(&f.removed, id)
	return nil
}

func newTestSandbox(t *testing.T, daemon Daemon, inForce *capsule.Sandbox) *Manager {
	t.Helper()
	return &Manager{
		log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		daemon:     daemon,
		instanceID: "instance-0001",
		probeImage: "probe",
		state:      newSandboxState(inForce),
	}
}

// TestParseHostCIDRs reads what the probe actually prints. Each address
// is masked to its network, because the deny set is about ranges and an
// interface address would deny one host of them.
func TestParseHostCIDRs(t *testing.T) {
	out := `1: lo    inet 127.0.0.1/8 scope host lo
2: eth0    inet 10.0.5.37/24 brd 10.0.5.255 scope global eth0
3: eth1    inet 10.0.5.99/24 brd 10.0.5.255 scope global eth1
4: docker0    inet 172.17.0.1/16 scope global docker0`
	got, err := parseHostCIDRs(out)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"127.0.0.0/8", "10.0.5.0/24", "172.17.0.0/16"}
	if !slices.Equal(got, want) {
		t.Errorf("parseHostCIDRs = %v; want networks, deduplicated, in order: %v", got, want)
	}

	if _, err := parseHostCIDRs("7: eth0    inet not-an-address scope global"); err == nil {
		t.Error("an unparseable line was accepted; a deny set must not be built from a line nobody read")
	}
	if got, err := parseHostCIDRs(""); err != nil || len(got) != 0 {
		t.Errorf("empty output = (%v, %v); it is the caller that decides emptiness is fatal", got, err)
	}
}

// TestBuildRefusesABlindDenySet: a probe that reports no global network
// has not proved the host has none, and a deny set built from it would
// allow every range it could not see. Failing closed is the only safe
// reading, and it is why serve does not start.
func TestBuildRefusesABlindDenySet(t *testing.T) {
	// `ip ... scope global` filters, so a probe that saw nothing prints
	// nothing. Scope is not re-checked here — the command is the filter.
	n := newTestSandbox(t, &fakeDaemon{
		uplinkID: "up-1", uplinkSubnet: "172.30.0.0/24", probeOut: "",
	}, nil)
	if _, err := n.build(t.Context(), sandboxRefreshBudget); err == nil {
		t.Fatal("a sandbox was built from a probe that saw no global network")
	}
}

// TestBuildDeniesEverythingItDiscovered: the uplink, the host's networks,
// every subnet the daemon knows and the operator's own list all have to
// reach the deny set. Any one of them missing is a hole in every
// capsule's egress, which is why this fails serve rather than warning.
func TestBuildDeniesEverythingItDiscovered(t *testing.T) {
	n := newTestSandbox(t, &fakeDaemon{
		uplinkID:     "up-1",
		uplinkSubnet: "172.30.0.0/24",
		subnets:      []string{"172.18.0.0/16"},
		probeOut:     "2: eth0    inet 192.0.2.10/24 scope global eth0",
	}, nil)
	n.denies = []string{"203.0.113.0/24"}
	n.allow = []string{"10.9.0.0/16"}

	sb, err := n.build(t.Context(), sandboxRefreshBudget)
	if err != nil {
		t.Fatal(err)
	}
	if sb.UplinkNetworkID != "up-1" || sb.UplinkSubnet != "172.30.0.0/24" {
		t.Errorf("uplink = %s %s; want the one it ensured", sb.UplinkNetworkID, sb.UplinkSubnet)
	}
	for _, want := range []string{"172.30.0.0/24", "192.0.2.0/24", "172.18.0.0/16", "203.0.113.0/24"} {
		if !slices.Contains(sb.Deny, want) {
			t.Errorf("deny set is missing %s: %v", want, sb.Deny)
		}
	}
	if !slices.Equal(sb.Allow, []string{"10.9.0.0/16"}) {
		t.Errorf("allow = %v; want the operator's list", sb.Allow)
	}
}

// TestApplyPolicyCostsWhatTheChangeWas is the asymmetry the whole
// classification exists for. A restriction that did not install leaves a
// capsule able to reach what the policy now denies, so those gateways are
// closed. A relaxation that did not install leaves it under the stricter
// policy it started with, so its work continues.
func TestApplyPolicyCostsWhatTheChangeWas(t *testing.T) {
	gateways := []engine.OwnedContainer{
		{ID: "gw-ok", Name: "gw-ok", Role: capsule.RoleGateway, Running: true},
		{ID: "gw-bad", Name: "gw-bad", Role: capsule.RoleGateway, Running: true},
		{ID: "gw-stopped", Name: "gw-stopped", Role: capsule.RoleGateway},
		{ID: "runner", Name: "runner", Role: capsule.RoleCapsule, Running: true},
	}
	inForce := &capsule.Sandbox{UplinkNetworkID: "up-1", Deny: []string{"10.0.0.0/8"}}

	t.Run("a restriction that did not land closes the gateway", func(t *testing.T) {
		d := &fakeDaemon{containers: gateways, refuse: map[string]bool{"gw-bad": true}}
		n := newTestSandbox(t, d, inForce)
		n.applyPolicy(t.Context(), &capsule.Sandbox{
			UplinkNetworkID: "up-1", Deny: []string{"10.0.0.0/8", "192.168.0.0/16"},
		})
		if !slices.Equal(d.reloads(), []string{"gw-ok"}) {
			t.Errorf("reloaded %v; only running gateways take a policy", d.reloads())
		}
		if !slices.Equal(d.removes(), []string{"gw-bad"}) {
			t.Errorf("removed %v; the gateway that refused a restriction must be closed", d.removes())
		}
	})

	t.Run("a relaxation that did not land leaves the work running", func(t *testing.T) {
		d := &fakeDaemon{containers: gateways, refuse: map[string]bool{"gw-bad": true}}
		n := newTestSandbox(t, d, inForce)
		n.applyPolicy(t.Context(), &capsule.Sandbox{UplinkNetworkID: "up-1"})
		if len(d.removes()) != 0 {
			t.Errorf("removed %v; a gateway keeping a stricter policy is not a danger", d.removes())
		}
	})

	t.Run("nothing changed installs nothing", func(t *testing.T) {
		d := &fakeDaemon{containers: gateways}
		n := newTestSandbox(t, d, inForce)
		n.applyPolicy(t.Context(), &capsule.Sandbox{UplinkNetworkID: "up-1", Deny: []string{"10.0.0.0/8"}})
		if len(d.reloads())+len(d.removes()) != 0 {
			t.Errorf("an unchanged policy touched %v / %v", d.reloads(), d.removes())
		}
	})

	t.Run("a recreated uplink is a change even with the same deny set", func(t *testing.T) {
		d := &fakeDaemon{containers: gateways}
		n := newTestSandbox(t, d, inForce)
		n.applyPolicy(t.Context(), &capsule.Sandbox{UplinkNetworkID: "up-2", Deny: []string{"10.0.0.0/8"}})
		if !slices.Equal(d.reloads(), []string{"gw-bad", "gw-ok"}) {
			t.Errorf("reloaded %v; a new uplink has to reach every gateway", d.reloads())
		}
	})
}

// TestCloseGatewaysRemovesEvenWhatWillNotDeny: closing a gateway asks it
// to revoke its own policy first, and removes the container either way.
//
// Both halves matter and only one was held. An established tunnel is
// already past the relay's check, so revoking from inside is not enough
// on its own and the container goes regardless — that is the half this
// test had. The other is that the revocation is attempted at all: going
// straight to the removal leaves the gateway relaying for as long as the
// daemon takes to answer, which is exactly the window the deny-all is
// there to close.
func TestCloseGatewaysRemovesEvenWhatWillNotDeny(t *testing.T) {
	d := &fakeDaemon{
		containers: []engine.OwnedContainer{
			{ID: "gw-1", Role: capsule.RoleGateway, Running: true},
			{ID: "gw-2", Role: capsule.RoleGateway, Running: true},
			{ID: "runner", Role: capsule.RoleCapsule, Running: true},
		},
		refuse: map[string]bool{"gw-2": true},
	}
	n := newTestSandbox(t, d, &capsule.Sandbox{})
	if err := n.closeGateways(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(d.removes(), []string{"gw-1", "gw-2"}) {
		t.Errorf("removed %v; a gateway that refuses deny-all still has to stop relaying", d.removes())
	}
	// Ordered, and naming the command. A close that removed first and
	// revoked afterwards, or that ran some other exec, leaves the
	// gateway relaying until the daemon answers — which is the window
	// the revocation exists to close, and neither shape is visible in a
	// sorted set of container ids.
	for _, id := range []string{"gw-1", "gw-2"} {
		want := []string{protocol.GatewayDenyAllCommand + ":" + id, "remove:" + id}
		if got := d.eventsFor(id); !slices.Equal(got, want) {
			t.Errorf("%s saw %v; want %v", id, got, want)
		}
	}
}

// A nil sandbox is the unsafe-open-egress profile, and every serving path
// asks it the same questions.
func TestANilSandboxIsTheOpenProfile(t *testing.T) {
	var n *Manager
	sb, err := n.ForLaunch(t.Context())
	if sb != nil || err != nil {
		t.Errorf("forLaunch on the open profile = (%v, %v); want no policy and no error", sb, err)
	}
	n.Watch(t.Context()) // returns rather than ticking forever
}

// TestNewNetworkSandboxFailsServeClosed. The constructor is where the
// first policy is computed, and serve does not start without a complete
// one: a capsule launched under a partial deny set can reach what the
// profile promises it cannot, and there is no later pass that would
// notice.
func TestNewNetworkSandboxFailsServeClosed(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Config{}
	config.ApplyDefaults(cfg)

	blind := &fakeDaemon{uplinkID: "up-1", uplinkSubnet: "172.30.0.0/24", probeOut: ""}
	if _, err := New(t.Context(), blind, "instance-0001", "probe", cfg, log); err == nil {
		t.Error("serve would have started on a deny set built from a probe that saw nothing")
	}

	seeing := &fakeDaemon{
		uplinkID: "up-1", uplinkSubnet: "172.30.0.0/24",
		probeOut: "2: eth0    inet 192.0.2.10/24 scope global eth0",
	}
	// The operator's own lists ride the constructor too. They are the
	// only mechanism for denying a range discovery cannot see, and the
	// only test that exercised them set the fields past the constructor
	// -- so the two loops that read the configuration could be deleted
	// and every deployment's denyCIDRs silently ignored.
	deny, err := config.ParseCIDR("100.64.0.0/10")
	if err != nil {
		t.Fatal(err)
	}
	allow, err := config.ParseCIDR("10.9.0.0/16")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Network.DenyCIDRs = []config.CIDR{deny}
	cfg.Network.AllowPrivateCIDRs = []config.CIDR{allow}
	n, err := New(t.Context(), seeing, "instance-0001", "probe", cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	// The snapshot a launch is cut from is in force from the start.
	sb, err := n.ForLaunch(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if sb == nil || sb.UplinkNetworkID != "up-1" {
		t.Errorf("the first launch would get %+v; want the policy just built", sb)
	}
	if !slices.Contains(sb.Deny, "100.64.0.0/10") {
		t.Errorf("deny = %v; the operator's configured range is not in it", sb.Deny)
	}
	if !slices.Contains(sb.Allow, "10.9.0.0/16") {
		t.Errorf("allow = %v; the operator's configured range is not in it", sb.Allow)
	}
}

// TestARestrictionThatCouldNotBeEnumeratedIsRetried: a policy is
// recorded as in force only once it has reached every gateway that
// could be named.
//
// The snapshot is what the next pass compares against. Recorded before
// the install, a pass whose enumeration failed leaves the new set in the
// books while no gateway ever received it — and the pass after that
// compares the new set against itself, reports it unchanged, and returns
// without trying. A restriction that landed nowhere is then never
// attempted again.
func TestARestrictionThatCouldNotBeEnumeratedIsRetried(t *testing.T) {
	inForce := &capsule.Sandbox{
		UplinkNetworkID: "uplink-1",
		UplinkSubnet:    "172.30.0.0/24",
		Deny:            []string{"10.0.0.0/8"},
	}
	daemon := &fakeDaemon{
		refuse:  map[string]bool{},
		listErr: errors.New("daemon unreachable"),
		containers: []engine.OwnedContainer{
			{ID: "gw-1", Role: capsule.RoleGateway, Running: true},
		},
	}
	n := newTestSandbox(t, daemon, inForce)

	tightened := &capsule.Sandbox{
		UplinkNetworkID: "uplink-1",
		UplinkSubnet:    "172.30.0.0/24",
		Deny:            []string{"10.0.0.0/8", "192.168.0.0/16"},
	}
	if err := n.applyPolicy(t.Context(), tightened); err == nil {
		t.Fatal("an install that could not name a single gateway reported success")
	}
	if got := n.state.snapshot().Deny; len(got) != 1 {
		t.Fatalf("the books record %v as in force; nothing was installed anywhere", got)
	}

	// The daemon recovers, and the pass that follows must see the change
	// again rather than compare the new set against itself.
	daemon.listErr = nil
	if err := n.applyPolicy(t.Context(), tightened); err != nil {
		t.Fatalf("the retry failed: %v", err)
	}
	if got := daemon.reloads(); len(got) != 1 || got[0] != "gw-1" {
		t.Errorf("gateways reloaded on the retry = %v; want the restriction to reach gw-1", got)
	}
}

// TestARefreshPaysItsExecBoundsInParallel: installing a policy overlaps
// the gateways it reaches, so the wait is the number of waves and not the
// number of gateways.
//
// The pass holds the refresh lock, and every launch waits on that lock
// with a plain mutex — no context, nothing to give up, no way to time
// out. So the pass's duration is a launch's worst-case wait. There is
// one gateway per running capsule, so walking them one at a time made
// that wait grow with exactly the parallelism the host was configured
// for: at thirty-two capsules and a thirty-second exec bound, sixteen
// minutes during which nothing new could start, for one policy change.
//
// The exec count is unchanged — still one per gateway. What changed is
// how many are outstanding at once, which is what the wait is made of:
// ceil(N/8) bounds rather than N.
func TestARefreshPaysItsExecBoundsInParallel(t *testing.T) {
	const (
		count   = 24
		perCall = 40 * time.Millisecond
	)
	// Two checks on the knob before the one on the code, because the
	// assertion below compares against the knob and so moves with it.
	//
	// A fan-out of one is not a narrower tuning, it is the serial pass
	// this replaces. Whether eight is the right width is a judgement
	// about how much a single daemon should be asked at once, and no test
	// can settle that; that it is more than one is not a judgement.
	if gatewayFanout < 2 {
		t.Fatalf("the fan-out bound is %d, which is the serial pass with extra steps", gatewayFanout)
	}
	// And the bound is only observable with more gateways than slots:
	// with fewer, a bounded pass and an unbounded one both run everything
	// at once and look identical.
	if gatewayFanout >= count {
		t.Fatalf("this test needs more gateways (%d) than the fan-out bound (%d), "+
			"or it cannot tell a bounded pass from an unbounded one", count, gatewayFanout)
	}

	containers := make([]engine.OwnedContainer, 0, count)
	for i := range count {
		id := fmt.Sprintf("gw-%02d", i)
		containers = append(containers, engine.OwnedContainer{
			ID: id, Name: id, Role: capsule.RoleGateway, Running: true,
		})
	}
	d := &fakeDaemon{containers: containers, delay: perCall}
	n := newTestSandbox(t, d, &capsule.Sandbox{UplinkNetworkID: "up-1"})

	failed, err := n.reloadGateways(t.Context(), nil, []string{"10.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}
	if len(failed) != 0 {
		t.Fatalf("gateways refused the policy: %v", failed)
	}
	// Every one of them, so a pass that is cheap because it skipped work
	// cannot pass for a pass that is cheap because it fanned out.
	if got := len(d.reloads()); got != count {
		t.Fatalf("reloaded %d of %d gateways", got, count)
	}

	// What is asserted is how many commands the daemon served at once,
	// not how long the pass took. Wall-clock is a machine-speed test: it
	// passes on an idle laptop and fails under coverage instrumentation,
	// and neither says anything about the code.
	//
	// Exactly the bound, in both directions: fewer means the slots are
	// not being filled and launches wait out more waves than they should,
	// more means the bound is not holding at all.
	if peak, want := d.peakInFlight(), gatewayFanout; peak != want {
		t.Errorf("peak %d gateway command(s) in flight for %d gateways; want %d. "+
			"Fewer means launches wait out more waves than they should; more means the "+
			"bound is not holding, and a pass that opens one exec per capsule trades a "+
			"slow refresh for a slow daemon", peak, count, want)
	}
}

// TestClosingAGatewayIsBoundedInBothSteps: closing a gateway bounds its
// removal, not only the exec that precedes it.
//
// Both steps run under the refresh lock, and every launch waits on that
// lock with a plain mutex — no context, nothing to give up, no way to
// time out. A daemon that accepts a container removal and then answers
// nothing is an ordinary failure, and an unbounded one there holds that
// lock for the life of the process while every launch on the host blocks
// behind it. Bounding the exec and not the removal left that in the same
// function as the bound.
func TestClosingAGatewayIsBoundedInBothSteps(t *testing.T) {
	d := &fakeDaemon{containers: []engine.OwnedContainer{
		{ID: "gw-1", Name: "gw-1", Role: capsule.RoleGateway, Running: true},
	}}
	n := newTestSandbox(t, d, &capsule.Sandbox{UplinkNetworkID: "up-1"})

	// A context with no deadline of its own, which is what the watch
	// loop hands this path.
	if err := n.closeGateway(context.Background(), "gw-1"); err != nil {
		t.Fatalf("close: %v", err)
	}
	if !slices.Equal(d.removes(), []string{"gw-1"}) {
		t.Fatalf("removed %v; want gw-1", d.removes())
	}

	d.mu.Lock()
	bounded, deadline := d.removeBounded, d.removeDeadline
	d.mu.Unlock()
	if !bounded {
		t.Fatal("the removal ran on a context with no deadline; a daemon that stops " +
			"answering holds the refresh lock, and every launch waits on it uninterruptibly")
	}
	if left := time.Until(deadline); left <= 0 || left > gatewayControlTimeout {
		t.Errorf("the removal had %v left; want a positive bound no larger than %v",
			left, gatewayControlTimeout)
	}
}

// TestDiscoveringTheEnvironmentIsBounded: building the sandbox runs
// under a deadline, because the refresh lock is held across it.
//
// The host probe is a container: created, started, waited on, and read.
// Waiting on a process that never exits is not a request that times out
// on its own, and the three daemon calls around it are no better against
// a daemon that has stopped answering. Every launch takes the refresh
// lock through forLaunch with a plain mutex — no context, no way out —
// so anything unbounded here is a host on which nothing new can start
// for as long as the controller runs.
//
// The bound is asserted rather than waited out: a test that waits two
// minutes to prove a two-minute budget is a test nobody runs.
func TestDiscoveringTheEnvironmentIsBounded(t *testing.T) {
	d := &fakeDaemon{
		uplinkID:     "up-1",
		uplinkSubnet: "172.30.0.0/24",
		subnets:      []string{"172.18.0.0/16"},
		probeOut:     "2: eth0    inet 192.0.2.10/24 scope global eth0",
	}
	n := newTestSandbox(t, d, nil)

	// A context with no deadline of its own, which is what the watch
	// loop and a launch both hand this path.
	if _, err := n.build(context.Background(), sandboxRefreshBudget); err != nil {
		t.Fatalf("build: %v", err)
	}

	d.mu.Lock()
	bounded, deadline := d.buildBounded, d.buildDeadline
	d.mu.Unlock()
	if !bounded {
		t.Fatal("the host probe ran on a context with no deadline; a probe that never exits " +
			"holds the refresh lock, and every launch waits on it uninterruptibly")
	}
	if left := time.Until(deadline); left <= 0 || left > sandboxRefreshBudget {
		t.Errorf("the probe had %v left; want a positive bound no larger than %v",
			left, sandboxRefreshBudget)
	}
	if sandboxRefreshBudget >= rediscoverInterval {
		t.Errorf("the build budget is %v against a %v rediscovery interval; a build that "+
			"hangs would leave two passes overlapping", sandboxRefreshBudget, rediscoverInterval)
	}
}

// TestEnumeratingGatewaysIsBounded: asking the daemon what this instance
// owns runs under a deadline.
//
// It is the first thing both passes over the gateways do, and both run
// inside the refresh lock. A daemon that accepts the request and answers
// nothing would hold that lock before a single gateway had been reached,
// with every launch on the host waiting on a plain mutex it cannot time
// out of. Bounding the exec and the removal further in, and not this,
// left the pass unbounded at its very first step.
func TestEnumeratingGatewaysIsBounded(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*Manager) error
	}{
		{"reloading", func(n *Manager) error {
			_, err := n.reloadGateways(context.Background(), nil, []string{"10.0.0.0/8"})
			return err
		}},
		{"closing", func(n *Manager) error {
			return n.closeGateways(context.Background())
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &fakeDaemon{containers: []engine.OwnedContainer{
				{ID: "gw-1", Name: "gw-1", Role: capsule.RoleGateway, Running: true},
			}}
			n := newTestSandbox(t, d, &capsule.Sandbox{UplinkNetworkID: "up-1"})

			// A context with no deadline of its own, which is what the
			// watch loop hands this path.
			if err := tc.run(n); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			d.mu.Lock()
			bounded, deadline := d.listBounded, d.listDeadline
			d.mu.Unlock()
			if !bounded {
				t.Fatal("the enumeration ran on a context with no deadline; a daemon that " +
					"stops answering holds the refresh lock before any gateway is reached")
			}
			if left := time.Until(deadline); left <= 0 || left > gatewayControlTimeout {
				t.Errorf("the enumeration had %v left; want a positive bound no larger than %v",
					left, gatewayControlTimeout)
			}
		})
	}
}

// TestARefreshGivesUpWhenItsCallerIsDone: waiting to refresh has to be
// abandonable, because the wait is on work that outlives the caller.
//
// A launch holds the refresh while it discovers the daemon and reloads
// every gateway, on a context deliberately derived from no parent so its
// cleanup still runs after a shutdown begins. The pass that maintains
// the policy is a serve loop, and shutdown waits for the serve loops
// without a bound — the budget is a claim about what they cost, not a
// timer. Parked on a lock, that loop does not stop when its context is
// cancelled: it stops when a launch it cannot see finishes, and past the
// deployment's grace period the difference is a SIGKILL, which leaves
// every message session open for the next start to wait out.
func TestARefreshGivesUpWhenItsCallerIsDone(t *testing.T) {
	n := newTestSandbox(t, &fakeDaemon{}, &capsule.Sandbox{})

	// Stand in for the launch that is holding it.
	n.state.refreshing <- struct{}{}
	defer func() { <-n.state.refreshing }()

	ctx, cancel := context.WithCancel(context.Background())
	gaveUp := make(chan error, 1)
	go func() { gaveUp <- n.refresh(ctx) }()

	select {
	case err := <-gaveUp:
		t.Fatalf("the refresh did not wait for the one in flight: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	cancel()
	select {
	case err := <-gaveUp:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("gave up with %v; want the caller's own cancellation", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the refresh never gave up; shutdown waits on a launch it cannot see, " +
			"and the grace period ends in a kill with every session still open")
	}
}

// TestARediscoveryThatIsStoppedDoesNotAnnounceAnEmergency: a pass that
// is being stopped is not a pass that failed.
//
// A discovery that cannot say what is installed closes every gateway,
// and says so at error level, because a capsule may be reaching
// something the policy now denies. On the way down neither half holds.
// Against a real daemon the attempt cannot even be made — the context it
// would travel on is already done — so all that is left is an error line
// announcing that every gateway was cut, on every ordinary shutdown that
// lands inside a rediscovery, for something that provably did not
// happen. The watch loop waits for the refresh slot for much of its
// life, so that is not a rare landing.
//
// What is asserted here is therefore that the pass does not try. The
// daemon answers, so a pass that reached it would close and remove the
// gateway, and the announcement would be the one thing about the whole
// sequence that was true.
func TestARediscoveryThatIsStoppedDoesNotAnnounceAnEmergency(t *testing.T) {
	// A gateway to close, so "nothing was closed" is an observation
	// rather than an empty daemon answering emptily.
	daemon := &fakeDaemon{
		containers: []engine.OwnedContainer{{ID: "gw-1", Role: capsule.RoleGateway, LeaseID: "lse-1", Running: true}},
	}
	n := newTestSandbox(t, daemon, &capsule.Sandbox{})

	// Held by a launch, which is what leaves the pass waiting.
	n.state.refreshing <- struct{}{}
	defer func() { <-n.state.refreshing }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	n.rediscover(ctx)

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.events) != 0 || len(daemon.removed) != 0 {
		t.Errorf("a stopped rediscovery reached the daemon: %v, removed %v",
			daemon.events, daemon.removed)
	}
}

// tighter returns the deny set of s with one more range in it.
func tighter(s *capsule.Sandbox) *capsule.Sandbox {
	next := *s
	next.Deny = append(slices.Clone(s.Deny), "192.168.0.0/16")
	return &next
}

func sandboxFixture() *capsule.Sandbox {
	return &capsule.Sandbox{
		UplinkNetworkID: "uplink-1",
		UplinkSubnet:    "172.30.0.0/24",
		Deny:            []string{"10.0.0.0/8"},
	}
}

// TestAGatewayCreatedDuringATighteningDoesNotKeepTheOlderPolicy: a pass
// fans a change out to the gateways it can enumerate, and one still being
// created is not among them. Nothing comes back for it afterwards either,
// because the pass records the new set as in force and every later pass
// compares that set against itself. The launch that created the gateway
// is what closes the gap, before its capsule is authorized to start.
func TestAGatewayCreatedDuringATighteningDoesNotKeepTheOlderPolicy(t *testing.T) {
	daemon := &fakeDaemon{
		refuse: map[string]bool{},
		containers: []engine.OwnedContainer{
			{ID: "gw-1", Role: capsule.RoleGateway, Running: true, LeaseID: "lse-1"},
		},
	}
	// The launch's gateway appears just after the pass has listed, which
	// is the whole of the window this closes.
	daemon.onList = func(f *fakeDaemon) {
		f.containers = append(f.containers, engine.OwnedContainer{
			ID: "gw-late", Role: capsule.RoleGateway, Running: true, LeaseID: "lse-late"})
		f.onList = nil
	}
	n := newTestSandbox(t, daemon, sandboxFixture())
	launched := n.state.snapshot()

	if err := n.applyPolicy(t.Context(), tighter(launched)); err != nil {
		t.Fatalf("applyPolicy: %v", err)
	}
	if got := daemon.reloads(); len(got) != 1 || got[0] != "gw-1" {
		t.Fatalf("the pass reloaded %v; a gateway it never named cannot be among them", got)
	}

	if err := n.ConfirmLaunch(t.Context(), "lse-late", launched); err != nil {
		t.Fatalf("confirmLaunch: %v", err)
	}
	if got := daemon.reloads(); !slices.Contains(got, "gw-late") {
		t.Errorf("reloaded %v; the launch must install the set in force into the gateway it created", got)
	}
}

// TestAConfirmedLaunchUnderAnUnchangedPolicyTouchesNoDaemon: the ordinary
// case is that nothing moved while the capsule was being built, and it
// has to stay free. Re-asserting the policy on every launch instead would
// cost one exec per gateway per launch, which is the cost the fan-out is
// bounded to avoid.
func TestAConfirmedLaunchUnderAnUnchangedPolicyTouchesNoDaemon(t *testing.T) {
	daemon := &fakeDaemon{
		refuse: map[string]bool{},
		containers: []engine.OwnedContainer{
			{ID: "gw-1", Role: capsule.RoleGateway, Running: true, LeaseID: "lse-1"},
		},
	}
	n := newTestSandbox(t, daemon, sandboxFixture())

	if err := n.ConfirmLaunch(t.Context(), "lse-1", n.state.snapshot()); err != nil {
		t.Fatalf("confirmLaunch: %v", err)
	}
	if got := daemon.reloads(); len(got) != 0 {
		t.Errorf("reloaded %v; a launch whose policy did not move reaches no gateway", got)
	}
	if daemon.lists != 0 {
		t.Errorf("the daemon was enumerated %d times; a launch whose policy did not move does not ask",
			daemon.lists)
	}
}

// TestAConfirmationThatCannotReachTheGatewayFailsTheLaunch: an unprovable
// policy refuses a launch everywhere else in this file, and a gateway that
// will not take the set in force is exactly that. The teardown belongs to
// the launch's own recovery, which removes every resource of the lease --
// so this must not remove anything itself.
func TestAConfirmationThatCannotReachTheGatewayFailsTheLaunch(t *testing.T) {
	launched := sandboxFixture()
	for name, tc := range map[string]struct {
		containers []engine.OwnedContainer
		refuse     map[string]bool
	}{
		"the gateway refuses the policy in force": {
			containers: []engine.OwnedContainer{
				{ID: "gw-late", Role: capsule.RoleGateway, Running: true, LeaseID: "lse-late"},
			},
			refuse: map[string]bool{"gw-late": true},
		},
		"the lease owns no running gateway": {
			containers: []engine.OwnedContainer{
				{ID: "gw-other", Role: capsule.RoleGateway, Running: true, LeaseID: "lse-other"},
			},
			refuse: map[string]bool{},
		},
	} {
		t.Run(name, func(t *testing.T) {
			daemon := &fakeDaemon{refuse: tc.refuse, containers: tc.containers}
			n := newTestSandbox(t, daemon, tighter(launched))

			if err := n.ConfirmLaunch(t.Context(), "lse-late", launched); err == nil {
				t.Fatal("the launch was confirmed against a policy no gateway of its own carries")
			}
			if got := daemon.removes(); len(got) != 0 {
				t.Errorf("removed %v; the launch's own recovery owns the teardown", got)
			}
		})
	}
}

// TestAConfirmationThatWasOvertakenFailsTheLaunch: a pass that moved the
// set while the confirmation ran leaves the gateway one generation behind,
// and could not have reached it either. A capsule must not start under a
// set that was current a moment ago.
func TestAConfirmationThatWasOvertakenFailsTheLaunch(t *testing.T) {
	launched := sandboxFixture()
	daemon := &fakeDaemon{
		refuse: map[string]bool{},
		containers: []engine.OwnedContainer{
			{ID: "gw-late", Role: capsule.RoleGateway, Running: true, LeaseID: "lse-late"},
		},
	}
	n := newTestSandbox(t, daemon, tighter(launched))
	daemon.onList = func(f *fakeDaemon) {
		f.onList = nil
		moved := *launched
		moved.Deny = []string{"10.0.0.0/8", "192.168.0.0/16", "172.16.0.0/12"}
		n.state.replace(&moved)
	}

	if err := n.ConfirmLaunch(t.Context(), "lse-late", launched); err == nil {
		t.Fatal("the launch was confirmed against a set that had already been superseded")
	}
}

// TestARestrictionThatCouldNotBeClosedIsRetried: a restriction that
// reached no gateway and could not close it either is not in force. The
// books must not move, for the same reason an enumeration failure leaves
// them alone -- recorded, the next pass compares the new set against
// itself and never attempts it again, while a gateway keeps relaying past
// a deny the operator was promised.
func TestARestrictionThatCouldNotBeClosedIsRetried(t *testing.T) {
	daemon := &fakeDaemon{
		refuse:    map[string]bool{"gw-bad": true},
		removeErr: map[string]error{"gw-bad": errors.New("container is wedged")},
		containers: []engine.OwnedContainer{
			{ID: "gw-bad", Role: capsule.RoleGateway, Running: true, LeaseID: "lse-bad"},
		},
	}
	n := newTestSandbox(t, daemon, sandboxFixture())
	tightened := tighter(sandboxFixture())

	if err := n.applyPolicy(t.Context(), tightened); err == nil {
		t.Fatal("a restriction that reached no gateway and closed none reported success")
	}
	if got := n.state.snapshot().Deny; len(got) != 1 {
		t.Fatalf("the books record %v as in force; it is in no gateway", got)
	}

	// The daemon recovers, and the pass that follows must see the change
	// again rather than compare the new set against itself.
	daemon.refuse = map[string]bool{}
	daemon.removeErr = nil
	if err := n.applyPolicy(t.Context(), tightened); err != nil {
		t.Fatalf("the retry failed: %v", err)
	}
	if got := n.state.snapshot().Deny; len(got) != 2 {
		t.Errorf("the books record %v after a retry that reached the gateway", got)
	}
}

// TestRediscoverClosesEveryGatewayWhenDiscoveryFails. The policy in force
// is not a safe fallback, only an older one: a network that appeared
// since it was computed is reachable and there is no way to tell. So a
// discovery that cannot be trusted closes every gateway rather than
// relaying under a set that cannot be shown to be current.
func TestRediscoverClosesEveryGatewayWhenDiscoveryFails(t *testing.T) {
	d := &fakeDaemon{
		uplinkID:     "up-1",
		uplinkSubnet: "172.30.0.0/24",
		probeOut:     "", // saw nothing: the deny set cannot be trusted
		containers: []engine.OwnedContainer{
			{ID: "gw-1", Role: capsule.RoleGateway, Running: true},
			{ID: "gw-2", Role: capsule.RoleGateway, Running: true},
		},
	}
	n := newTestSandbox(t, d, &capsule.Sandbox{UplinkNetworkID: "up-1"})

	n.rediscover(t.Context())

	// Sorted: closing fans out, so the order is the order the daemon
	// answered in and nothing should depend on it.
	if !slices.Equal(d.removes(), []string{"gw-1", "gw-2"}) {
		t.Errorf("removed %v; a failed rediscovery has to close every gateway", d.removes())
	}
}
