package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"slices"
	"testing"

	"github.com/rhobuild/runpool/internal/capsule"
	"github.com/rhobuild/runpool/internal/config"
	"github.com/rhobuild/runpool/internal/platform/docker"

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

// fakeSandboxDaemon answers for both halves of the daemon the sandbox
// uses. Its point is the states that decide the design: a probe that sees
// nothing, and a gateway that refuses the policy it is handed.
type fakeSandboxDaemon struct {
	uplinkID     string
	uplinkSubnet string
	subnets      []string
	probeOut     string
	probeErr     error

	containers []docker.OwnedContainer
	// listErr is the daemon refusing to say which gateways exist, which
	// is the one failure that makes the whole install unaccountable.
	listErr error
	// refuse names the containers whose exec fails, which is how a
	// gateway that will not take a new policy is expressed.
	refuse   map[string]bool
	reloaded []string
	denied   []string
	removed  []string
}

func (f *fakeSandboxDaemon) EnsureOwnedNetwork(context.Context, docker.NetworkSpec) (string, error) {
	return f.uplinkID, nil
}
func (f *fakeSandboxDaemon) NetworkSubnet(context.Context, string) (string, error) {
	return f.uplinkSubnet, nil
}
func (f *fakeSandboxDaemon) AllNetworkSubnets(context.Context) ([]string, error) {
	return f.subnets, nil
}
func (f *fakeSandboxDaemon) RunTask(context.Context, docker.ContainerSpec) (int64, string, error) {
	return 0, f.probeOut, f.probeErr
}
func (f *fakeSandboxDaemon) ListOwnedContainers(context.Context, assignment.InstanceID) ([]docker.OwnedContainer, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.containers, nil
}
func (f *fakeSandboxDaemon) Exec(_ context.Context, id string, _ []string) (int, string, error) {
	f.denied = append(f.denied, id)
	if f.refuse[id] {
		return 1, "refused", nil
	}
	return 0, "", nil
}
func (f *fakeSandboxDaemon) ExecWithInput(_ context.Context, id string, _ []string, _ []byte) (int, string, error) {
	if f.refuse[id] {
		return 1, "refused", nil
	}
	f.reloaded = append(f.reloaded, id)
	return 0, "", nil
}
func (f *fakeSandboxDaemon) RemoveContainer(_ context.Context, id string) error {
	f.removed = append(f.removed, id)
	return nil
}

func newTestSandbox(t *testing.T, daemon sandboxDaemon, inForce *capsule.Sandbox) *networkSandbox {
	t.Helper()
	return &networkSandbox{
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
	n := newTestSandbox(t, &fakeSandboxDaemon{
		uplinkID: "up-1", uplinkSubnet: "172.30.0.0/24", probeOut: "",
	}, nil)
	if _, err := n.build(t.Context()); err == nil {
		t.Fatal("a sandbox was built from a probe that saw no global network")
	}
}

// TestBuildDeniesEverythingItDiscovered: the uplink, the host's networks,
// every subnet the daemon knows and the operator's own list all have to
// reach the deny set. Any one of them missing is a hole in every
// capsule's egress, which is why this fails serve rather than warning.
func TestBuildDeniesEverythingItDiscovered(t *testing.T) {
	n := newTestSandbox(t, &fakeSandboxDaemon{
		uplinkID:     "up-1",
		uplinkSubnet: "172.30.0.0/24",
		subnets:      []string{"172.18.0.0/16"},
		probeOut:     "2: eth0    inet 192.0.2.10/24 scope global eth0",
	}, nil)
	n.denies = []string{"203.0.113.0/24"}
	n.allow = []string{"10.9.0.0/16"}

	sb, err := n.build(t.Context())
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
	gateways := []docker.OwnedContainer{
		{ID: "gw-ok", Name: "gw-ok", Role: capsule.RoleGateway, Running: true},
		{ID: "gw-bad", Name: "gw-bad", Role: capsule.RoleGateway, Running: true},
		{ID: "gw-stopped", Name: "gw-stopped", Role: capsule.RoleGateway},
		{ID: "runner", Name: "runner", Role: capsule.RoleCapsule, Running: true},
	}
	inForce := &capsule.Sandbox{UplinkNetworkID: "up-1", Deny: []string{"10.0.0.0/8"}}

	t.Run("a restriction that did not land closes the gateway", func(t *testing.T) {
		d := &fakeSandboxDaemon{containers: gateways, refuse: map[string]bool{"gw-bad": true}}
		n := newTestSandbox(t, d, inForce)
		n.applyPolicy(t.Context(), &capsule.Sandbox{
			UplinkNetworkID: "up-1", Deny: []string{"10.0.0.0/8", "192.168.0.0/16"},
		})
		if !slices.Equal(d.reloaded, []string{"gw-ok"}) {
			t.Errorf("reloaded %v; only running gateways take a policy", d.reloaded)
		}
		if !slices.Equal(d.removed, []string{"gw-bad"}) {
			t.Errorf("removed %v; the gateway that refused a restriction must be closed", d.removed)
		}
	})

	t.Run("a relaxation that did not land leaves the work running", func(t *testing.T) {
		d := &fakeSandboxDaemon{containers: gateways, refuse: map[string]bool{"gw-bad": true}}
		n := newTestSandbox(t, d, inForce)
		n.applyPolicy(t.Context(), &capsule.Sandbox{UplinkNetworkID: "up-1"})
		if len(d.removed) != 0 {
			t.Errorf("removed %v; a gateway keeping a stricter policy is not a danger", d.removed)
		}
	})

	t.Run("nothing changed installs nothing", func(t *testing.T) {
		d := &fakeSandboxDaemon{containers: gateways}
		n := newTestSandbox(t, d, inForce)
		n.applyPolicy(t.Context(), &capsule.Sandbox{UplinkNetworkID: "up-1", Deny: []string{"10.0.0.0/8"}})
		if len(d.reloaded)+len(d.removed) != 0 {
			t.Errorf("an unchanged policy touched %v / %v", d.reloaded, d.removed)
		}
	})

	t.Run("a recreated uplink is a change even with the same deny set", func(t *testing.T) {
		d := &fakeSandboxDaemon{containers: gateways}
		n := newTestSandbox(t, d, inForce)
		n.applyPolicy(t.Context(), &capsule.Sandbox{UplinkNetworkID: "up-2", Deny: []string{"10.0.0.0/8"}})
		if !slices.Equal(d.reloaded, []string{"gw-ok", "gw-bad"}) {
			t.Errorf("reloaded %v; a new uplink has to reach every gateway", d.reloaded)
		}
	})
}

// TestCloseGatewaysRemovesEvenWhatWillNotDeny: an established tunnel is
// already past the relay's check, so closing the policy from inside is
// not enough on its own. The container goes either way.
func TestCloseGatewaysRemovesEvenWhatWillNotDeny(t *testing.T) {
	d := &fakeSandboxDaemon{
		containers: []docker.OwnedContainer{
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
	if !slices.Equal(d.removed, []string{"gw-1", "gw-2"}) {
		t.Errorf("removed %v; a gateway that refuses deny-all still has to stop relaying", d.removed)
	}
}

// A nil sandbox is the unsafe-open-egress profile, and every serving path
// asks it the same questions.
func TestANilSandboxIsTheOpenProfile(t *testing.T) {
	var n *networkSandbox
	sb, err := n.forLaunch(t.Context())
	if sb != nil || err != nil {
		t.Errorf("forLaunch on the open profile = (%v, %v); want no policy and no error", sb, err)
	}
	n.watch(t.Context()) // returns rather than ticking forever
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

	blind := &fakeSandboxDaemon{uplinkID: "up-1", uplinkSubnet: "172.30.0.0/24", probeOut: ""}
	if _, err := newNetworkSandbox(t.Context(), blind, "instance-0001", "probe", cfg, log); err == nil {
		t.Error("serve would have started on a deny set built from a probe that saw nothing")
	}

	seeing := &fakeSandboxDaemon{
		uplinkID: "up-1", uplinkSubnet: "172.30.0.0/24",
		probeOut: "2: eth0    inet 192.0.2.10/24 scope global eth0",
	}
	n, err := newNetworkSandbox(t.Context(), seeing, "instance-0001", "probe", cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	// The snapshot a launch is cut from is in force from the start.
	sb, err := n.forLaunch(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if sb == nil || sb.UplinkNetworkID != "up-1" {
		t.Errorf("the first launch would get %+v; want the policy just built", sb)
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
	daemon := &fakeSandboxDaemon{
		refuse:  map[string]bool{},
		listErr: errors.New("daemon unreachable"),
		containers: []docker.OwnedContainer{
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
	if len(daemon.reloaded) != 1 || daemon.reloaded[0] != "gw-1" {
		t.Errorf("gateways reloaded on the retry = %v; want the restriction to reach gw-1", daemon.reloaded)
	}
}
