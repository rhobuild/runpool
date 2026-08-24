package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rhobuild/runpool/internal/config"
	"github.com/rhobuild/runpool/internal/credential"
	"github.com/rhobuild/runpool/internal/engine"
	"github.com/rhobuild/runpool/internal/platform"
)

func TestReportOK(t *testing.T) {
	cases := map[string]struct {
		results []Result
		want    bool
	}{
		"all pass":       {[]Result{{Status: Pass}, {Status: Pass}}, true},
		"warn tolerated": {[]Result{{Status: Pass}, {Status: Warn}}, true},
		"one fail":       {[]Result{{Status: Pass}, {Status: Fail}}, false},
		"empty":          {nil, true},
	}
	for name, tc := range cases {
		if got := (Report{Results: tc.results}).OK(); got != tc.want {
			t.Errorf("%s: OK() = %v; want %v", name, got, tc.want)
		}
	}
}

func TestCheckHostTopology(t *testing.T) {
	shared := &config.Config{Host: config.Host{Topology: config.HostTopologySharedDaemon}}
	if got := checkHostTopology(shared); got.Status != Warn || !strings.Contains(got.Detail, "shared") || got.Fix == "" {
		t.Fatalf("shared topology = %+v; want an actionable warning", got)
	}
	dedicated := &config.Config{Host: config.Host{Topology: config.HostTopologyDedicatedDaemon}}
	if got := checkHostTopology(dedicated); got.Status != Pass {
		t.Fatalf("dedicated topology = %+v; want pass", got)
	}
}

func TestCheckStorage(t *testing.T) {
	dir := t.TempDir()
	if res := checkStorage("state", filepath.Join(dir, "created")); res.Status != Pass {
		t.Errorf("writable dir = %+v; want pass", res)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "created"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("storage probe left %d files behind", len(entries))
	}
	if res := checkStorage("state", ""); res.Status != Fail {
		t.Errorf("unset dir = %+v; want fail", res)
	}

	// A read-only mount is the failure this check exists for, so it must
	// be caught by writing, not by stat.
	readonly := filepath.Join(dir, "readonly")
	if err := os.Mkdir(readonly, 0o500); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root: permissions do not deny writes")
	}
	res := checkStorage("cache", readonly)
	if res.Status != Fail {
		t.Errorf("read-only dir = %+v; want fail", res)
	}
}

func TestCheckCapacity(t *testing.T) {
	cfg := &config.Config{
		Targets: []config.Target{
			{ID: "a", Tiers: []config.TierBinding{{TierID: "std"}}},
			{ID: "b", Tiers: []config.TierBinding{{TierID: "std"}}},
		},
		Tiers: []config.Tier{{ID: "std", Parallelism: 2}},
	}
	if res := checkCapacity(cfg); res.Status != Pass {
		t.Errorf("two bindings with parallelism two = %+v; want pass", res)
	}

	// More bindings than parallelism is legal under credits — they share the
	// tier and take turns at discovery — but it costs first-job
	// latency, so the operator is told rather than stopped.
	cfg.Tiers[0].Parallelism = 1
	res := checkCapacity(cfg)
	if res.Status != Warn {
		t.Fatalf("two bindings with parallelism one = %+v; want warn, not a failure", res)
	}
	if res.Fix == "" {
		t.Error("a contended capacity check must tell the operator what it costs")
	}
}

// TestPhysicalCapacity: full tiers plus the reserve must fit in what
// the daemon reports, and the failure names which resource ran out.
func TestPhysicalCapacity(t *testing.T) {
	cfg := &config.Config{
		Tiers: []config.Tier{{ID: "std", Parallelism: 4, Resources: config.Resources{
			Memory: 2 << 30, CPU: 2_000_000_000,
		}}},
	}
	cfg.Host.Reserve.Memory = 2 << 30
	cfg.Host.Reserve.CPU = 1_000_000_000
	host := engine.HostInfo{NCPU: 12, MemTotalBytes: 16 << 30, SwapTotalKnown: true}

	// 4x2GiB + 2GiB = 10GiB of 16; 4x2 + 1 = 9 of 12 CPUs.
	if res := physicalCapacity(cfg, host); res.Status != Pass {
		t.Errorf("fitting config = %+v; want pass", res)
	}

	tight := host
	tight.MemTotalBytes = 8 << 30
	if res := physicalCapacity(cfg, tight); res.Status != Fail || !strings.Contains(res.Fix, "memory") {
		t.Errorf("over-committed memory = %+v; want fail naming memory", res)
	}

	tight = host
	tight.NCPU = 8
	if res := physicalCapacity(cfg, tight); res.Status != Fail || !strings.Contains(res.Fix, "CPU") {
		t.Errorf("over-committed cpu = %+v; want fail naming CPU", res)
	}
}

func TestPhysicalCapacityUsesGlobalWorstCase(t *testing.T) {
	one := 1
	cfg := &config.Config{
		Scheduling: config.Scheduling{Parallelism: &one},
		Tiers: []config.Tier{
			{ID: "small", Parallelism: 1, Resources: config.Resources{Memory: 4 << 30, Swap: 512 << 20, CPU: 2e9}},
			{ID: "large", Parallelism: 1, Resources: config.Resources{Memory: 10 << 30, Swap: 2 << 30, CPU: 6e9}},
		},
		Host: config.Host{Reserve: config.Reserve{Memory: 5 << 30, Swap: 2 << 30, CPU: 2e9}},
	}
	host := engine.HostInfo{
		NCPU: 8, MemTotalBytes: 16 << 30, SwapTotalBytes: 4 << 30, SwapTotalKnown: true,
	}
	if res := physicalCapacity(cfg, host); res.Status != Pass {
		t.Fatalf("global parallelism one should budget the largest tier, not their sum: %+v", res)
	}

	cfg.Scheduling.Parallelism = nil
	if res := physicalCapacity(cfg, host); res.Status != Fail {
		t.Fatalf("independent tiers should budget both envelopes: %+v", res)
	}
}

func TestPhysicalCapacityRequiresKnownSufficientSwap(t *testing.T) {
	cfg := &config.Config{Tiers: []config.Tier{{
		ID: "std", Parallelism: 1,
		Resources: config.Resources{Memory: 1 << 30, Swap: 1 << 30, CPU: 1e9},
	}}}
	host := engine.HostInfo{NCPU: 2, MemTotalBytes: 4 << 30}
	if res := physicalCapacity(cfg, host); res.Status != Fail || !strings.Contains(res.Fix, "could not be read") {
		t.Fatalf("unknown swap = %+v; want fail", res)
	}
	host.SwapTotalKnown = true
	host.SwapTotalBytes = 512 << 20
	if res := physicalCapacity(cfg, host); res.Status != Fail || !strings.Contains(res.Fix, "exceeds host swap") {
		t.Fatalf("insufficient swap = %+v; want fail", res)
	}
}

// TestSwapGatesKeyOnDifferentQuestions pins the split the two swap checks
// deliberately make. A host reserve is capacity withheld from scheduling: it
// never reaches a container, so it must not demand cgroup swap accounting —
// but it does have to exist on the host.
func TestSwapGatesKeyOnDifferentQuestions(t *testing.T) {
	cfg := &config.Config{
		Tiers: []config.Tier{{
			ID: "std", Parallelism: 1,
			Resources: config.Resources{Memory: 1 << 30, Swap: 0, CPU: 1e9},
		}},
		Host: config.Host{Reserve: config.Reserve{Swap: 2 << 30}},
	}

	// No tier asks for swap, so Docker is never asked to cap it.
	if got := configuredSwap(cfg); got != 0 {
		t.Errorf("configuredSwap = %d with reserve-only swap; the cgroup gate must not fire", got)
	}

	// The host still has to own the swap the reserve withholds.
	host := engine.HostInfo{NCPU: 2, MemTotalBytes: 4 << 30, SwapTotalKnown: true, SwapTotalBytes: 1 << 30}
	if res := physicalCapacity(cfg, host); res.Status != Fail || !strings.Contains(res.Fix, "exceeds host swap") {
		t.Fatalf("reserve larger than host swap = %+v; want fail", res)
	}
	host.SwapTotalBytes = 4 << 30
	if res := physicalCapacity(cfg, host); res.Status != Pass {
		t.Fatalf("reserve within host swap = %+v; want pass", res)
	}

	// A tier that does ask for swap turns the cgroup gate on.
	cfg.Tiers[0].Resources.Swap = 1 << 30
	if got := configuredSwap(cfg); got != 1<<30 {
		t.Errorf("configuredSwap = %d; want the tier envelope", got)
	}
}

func TestMajorVersion(t *testing.T) {
	cases := map[string]int{
		"28.5.0":         28,
		"27.0.3":         27,
		"28":             28,
		"":               0,
		"v28.5":          0, // not a bare number: treated as unknown, which fails closed
		"garbage":        0,
		"28.5.0-ce+meta": 28,
	}
	for in, want := range cases {
		if got := majorVersion(in); got != want {
			t.Errorf("majorVersion(%q) = %d; want %d", in, got, want)
		}
	}
}

// TestEngineVersionGuardIsAFloorNotAWindow. The release-qualification
// reference names the host that produced a release's evidence; it is not a
// runtime constraint. A guard that turned one qualified patch into an equality
// would refuse daemons that run Runpool correctly, and a guard with a
// ceiling would refuse every future major sight unseen. Below the floor
// fails because the isolated gateway mode does not exist there; at or
// above it, the capability probe decides, not the number.
func TestEngineVersionGuardIsAFloorNotAWindow(t *testing.T) {
	for _, tc := range []struct {
		version   string
		wantAdmit bool
		why       string
	}{
		{"27.5.1", false, "the isolated bridge gateway mode does not exist before Engine 28"},
		{"28.0.0", true, "the compatibility floor itself is admitted"},
		{"28.5.0", true, "a compatible Engine 28 patch is admitted"},
		{"28.9.9", true, "a later 28 patch is not a drift to refuse"},
		{"29.0.0", true, "a newer major is admitted; the capability probe decides"},
		{"30.1.2", true, "no ceiling exists, so no future major is refused for its number"},
		{"", false, "an unreadable version fails closed"},
		{"garbage", false, "an unparseable version fails closed"},
	} {
		admitted := majorVersion(tc.version) >= platform.MinimumEngineMajor
		if admitted != tc.wantAdmit {
			t.Errorf("engine %q admitted=%v; want %v — %s", tc.version, admitted, tc.wantAdmit, tc.why)
		}
	}
}

// TestIsolatedBridgeProbeFailsClosedWithoutADaemon: the capability check
// is an admission gate, so an unusable daemon is a failure rather than a
// silently skipped check.
func TestIsolatedBridgeProbeFailsClosedWithoutADaemon(t *testing.T) {
	res := checkIsolatedBridge(t.Context(), nil, nil)
	if res.Status != Fail {
		t.Errorf("probe without a daemon = %s; want fail", res.Status)
	}
}

// TestProbeNamesAreUnique keeps two concurrent preflights on one host
// from colliding on the probe network's name.
func TestProbeNamesAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for range 64 {
		s, err := probeSuffix()
		if err != nil {
			t.Fatal(err)
		}
		if s == "" {
			t.Fatal("probe suffix is empty; the probe network would be unnamed")
		}
		if seen[s] {
			t.Fatalf("probe suffix %q repeated; concurrent preflights would collide", s)
		}
		seen[s] = true
	}
}

type fakeNetworkProbe struct {
	createErr    error
	removeErr    error
	cancel       context.CancelFunc
	created      engine.NetworkSpec
	removed      string
	removeCalls  int
	removeCtxErr error
	// gateways is what the daemon assigned the probe network. Under the
	// isolated mode it assigns none, so a non-empty answer is a daemon
	// that took the request and ignored it.
	gateways   []string
	inspectErr error
}

// TestTheBridgeProbeAsksWhatTheDaemonDidNotWhatItWasAsked: a create that
// succeeds proves the request was accepted, not that it was honoured.
//
// An option key the daemon does not recognise is dropped without
// complaint, so a build that renamed the option, or never had it,
// answers a create exactly like one that honours it. What differs is
// what the daemon then did: under the isolated mode it assigns the
// bridge no gateway, and the address a capsule would route through is
// the address that does not exist.
//
// The probe network is removed either way. Readiness runs on an
// interval, and a host that leaks one network per failing run runs out
// of address pools.
func TestTheBridgeProbeAsksWhatTheDaemonDidNotWhatItWasAsked(t *testing.T) {
	for name, testCase := range map[string]struct {
		gateways []string
		want     Status
		detail   string
	}{
		"no gateway is the mode taking effect": {
			want: Pass, detail: "no gateway address",
		},
		"a gateway is a daemon that took the request and ignored it": {
			gateways: []string{"172.19.0.1"},
			want:     Fail, detail: "172.19.0.1",
		},
	} {
		t.Run(name, func(t *testing.T) {
			probe := &fakeNetworkProbe{gateways: testCase.gateways}
			got := checkIsolatedBridge(t.Context(), probe, nil)

			if got.Status != testCase.want {
				t.Errorf("status = %v; want %v (%s)", got.Status, testCase.want, got.Detail)
			}
			if !strings.Contains(got.Detail, testCase.detail) {
				t.Errorf("detail = %q; it has to name %q", got.Detail, testCase.detail)
			}
			if probe.removeCalls != 1 {
				t.Errorf("the probe network was removed %d times; a host that leaks one per "+
					"readiness run runs out of address pools", probe.removeCalls)
			}
		})
	}
}

func (f *fakeNetworkProbe) NetworkGateways(context.Context, string) ([]string, error) {
	return f.gateways, f.inspectErr
}

func (f *fakeNetworkProbe) CreateNetwork(_ context.Context, spec engine.NetworkSpec) (string, error) {
	f.created = spec
	if f.cancel != nil {
		f.cancel()
	}
	return "probe-id", f.createErr
}

func (f *fakeNetworkProbe) RemoveNetwork(ctx context.Context, id string) error {
	f.removeCalls++
	f.removed = id
	f.removeCtxErr = ctx.Err()
	return f.removeErr
}

func TestIsolatedBridgeProbeLifecycle(t *testing.T) {
	t.Run("create failure", func(t *testing.T) {
		probe := &fakeNetworkProbe{createErr: errors.New("unsupported")}
		if got := checkIsolatedBridge(t.Context(), probe, nil); got.Status != Fail {
			t.Fatalf("status = %s; want fail", got.Status)
		}
		if probe.removeCalls != 1 {
			t.Fatalf("ambiguous create cleanup called %d times; want 1", probe.removeCalls)
		}
	})

	t.Run("cleanup survives caller cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		probe := &fakeNetworkProbe{cancel: cancel}
		if got := checkIsolatedBridge(ctx, probe, nil); got.Status != Pass {
			t.Fatalf("result = %+v; want pass", got)
		}
		if probe.removeCalls != 1 {
			t.Fatalf("remove calls = %d; want 1", probe.removeCalls)
		}
		if err := probe.removeCtxErr; err != nil {
			t.Fatalf("cleanup inherited cancellation: %v", err)
		}
		if !probe.created.Internal || !probe.created.Isolated {
			t.Fatalf("probe network = %+v; want internal and isolated", probe.created)
		}
		if probe.created.Labels["io.runpool.role"] != "preflight-probe" {
			t.Fatalf("probe role = %q; want preflight-probe", probe.created.Labels["io.runpool.role"])
		}
		if probe.removed != "probe-id" {
			t.Fatalf("removed %q; want the created network id", probe.removed)
		}
	})

	t.Run("cleanup failure closes admission", func(t *testing.T) {
		probe := &fakeNetworkProbe{removeErr: errors.New("busy")}
		if got := checkIsolatedBridge(t.Context(), probe, nil); got.Status != Fail {
			t.Fatalf("result = %+v; want fail", got)
		}
	})
}

// TestTheBridgeProbeDoesNotCloseAdmissionOnAProfileThatDoesNotUseIt:
// unsafe-open-egress builds no sandbox, so the isolated bridge is a
// capability that profile never asks the daemon for. Refusing a host
// over it would close admission on a configuration that is deliberate,
// documented and supported — and the probe is daemon work repeated on
// every readiness run for a network nothing would attach to.
func TestTheBridgeProbeDoesNotCloseAdmissionOnAProfileThatDoesNotUseIt(t *testing.T) {
	cfg := &config.Config{}
	cfg.Network.Profile = config.NetworkProfileUnsafeOpen
	probe := &fakeNetworkProbe{createErr: errors.New("unsupported")}

	got := checkIsolatedBridge(t.Context(), probe, cfg)

	if got.Status != Warn {
		t.Errorf("status = %s; want warn — this profile never attaches a capsule to the isolated bridge", got.Status)
	}
	if probe.created.Name != "" {
		t.Errorf("the probe created network %q; a profile that does not use the isolated bridge needs no probe", probe.created.Name)
	}
}

// fakeProbe answers as the provider would. It records the group it was
// asked for, because "" defaulting to "default" is a rule the operator
// never sees stated anywhere else.
type fakeProbe struct {
	asked string
	id    int
	err   error
	// sets are the scale sets the provider already holds, by name. A name
	// absent here is one the provider does not have yet, which is the
	// ordinary state before the first serve.
	sets    map[string]int
	setsErr error
}

func (f *fakeProbe) RunnerGroupID(_ context.Context, group string) (int, error) {
	f.asked = group
	return f.id, f.err
}

func (f *fakeProbe) ScaleSetID(_ context.Context, _, name string) (int, bool, error) {
	if f.setsErr != nil {
		return 0, false, f.setsErr
	}
	id, ok := f.sets[name]
	return id, ok, nil
}

// TestCheckCredentials is the check that needs a provider, and the reason
// the provider arrives as a factory the caller supplies. Every outcome an
// operator can hit is reachable without a network: the token missing, the
// client refusing to be built, and the permission failure that is the
// most common of all — a credential that authenticates but cannot see the
// runner group.
func TestCheckCredentials(t *testing.T) {
	cfg := &config.Config{
		Credentials: []config.Credential{{ID: "gh", TokenEnv: "TOKEN"}},
		Targets: []config.Target{
			{ID: "app", URL: "https://github.com/acme/app", CredentialID: "gh"},
		},
	}
	environ := func(k string) string {
		if k == "TOKEN" {
			return "t0ken"
		}
		return ""
	}

	t.Run("reachable", func(t *testing.T) {
		probe := &fakeProbe{id: 7}
		res := checkCredentials(t.Context(), Options{
			Config: cfg, Environ: environ,
			NewCredentialProbe: func(string, credential.Secret) (CredentialProbe, error) { return probe, nil },
		})
		if len(res) != 1 || res[0].Status != Pass {
			t.Fatalf("results = %+v; want one pass", res)
		}
		if probe.asked != "default" {
			t.Errorf("asked for runner group %q; an unset group means default", probe.asked)
		}
		if !strings.Contains(res[0].Detail, "repository") {
			t.Errorf("detail %q does not name the scope it proved", res[0].Detail)
		}
	})

	t.Run("group unreachable", func(t *testing.T) {
		res := checkCredentials(t.Context(), Options{
			Config: cfg, Environ: environ,
			NewCredentialProbe: func(string, credential.Secret) (CredentialProbe, error) {
				return &fakeProbe{err: errors.New("404 not found")}, nil
			},
		})
		if len(res) != 1 || res[0].Status != Fail {
			t.Fatalf("results = %+v; want one failure", res)
		}
		if !strings.Contains(res[0].Fix, "administration") {
			t.Errorf("fix %q does not say what permission is missing", res[0].Fix)
		}
	})

	t.Run("named group is honoured", func(t *testing.T) {
		withGroup := *cfg
		withGroup.Targets = []config.Target{
			{ID: "org", URL: "https://github.com/acme", CredentialID: "gh", RunnerGroup: "isolated"},
		}
		probe := &fakeProbe{id: 3}
		checkCredentials(t.Context(), Options{
			Config: &withGroup, Environ: environ,
			NewCredentialProbe: func(string, credential.Secret) (CredentialProbe, error) { return probe, nil },
		})
		if probe.asked != "isolated" {
			t.Errorf("asked for runner group %q; want the configured one", probe.asked)
		}
	})

	t.Run("token missing", func(t *testing.T) {
		res := checkCredentials(t.Context(), Options{
			Config: cfg, Environ: func(string) string { return "" },
			NewCredentialProbe: func(string, credential.Secret) (CredentialProbe, error) {
				t.Error("the provider was reached without a credential")
				return nil, nil
			},
		})
		if len(res) != 1 || res[0].Status != Fail {
			t.Fatalf("results = %+v; want one failure", res)
		}
	})

	t.Run("client refuses to be built", func(t *testing.T) {
		res := checkCredentials(t.Context(), Options{
			Config: cfg, Environ: environ,
			NewCredentialProbe: func(string, credential.Secret) (CredentialProbe, error) {
				return nil, errors.New("bad base url")
			},
		})
		if len(res) != 1 || res[0].Status != Fail {
			t.Fatalf("results = %+v; want one failure", res)
		}
	})
}

// TestCheckCredentialsNamesTheRunsOnLabel: a scale set is named in
// configuration and matched by a workflow's runs-on, and nothing else an
// operator can run says the two are the same string. A workflow that
// names it wrongly queues forever against an instance whose every other
// check passes, so the check that proves the credential also states the
// label.
func TestCheckCredentialsNamesTheRunsOnLabel(t *testing.T) {
	cfg := &config.Config{
		Credentials: []config.Credential{{ID: "gh", TokenEnv: "TOKEN"}},
		Targets: []config.Target{{
			ID: "app", URL: "https://github.com/acme/app", CredentialID: "gh",
			Tiers: []config.TierBinding{
				{TierID: "standard", ScaleSetName: "runpool-standard"},
				{TierID: "heavy", ScaleSetName: "runpool-heavy"},
			},
		}},
	}
	environ := func(k string) string {
		if k == "TOKEN" {
			return "t0ken"
		}
		return ""
	}
	run := func(p *fakeProbe) []Result {
		return checkCredentials(t.Context(), Options{
			Config: cfg, Environ: environ,
			NewCredentialProbe: func(string, credential.Secret) (CredentialProbe, error) { return p, nil },
		})
	}

	t.Run("one already exists, one does not", func(t *testing.T) {
		res := run(&fakeProbe{id: 7, sets: map[string]int{"runpool-standard": 42}})
		if len(res) != 3 {
			t.Fatalf("results = %+v; want the credential plus one per tier", res)
		}
		for _, r := range res {
			if r.Status != Pass {
				t.Errorf("%s is %v: %s", r.Name, r.Status, r.Detail)
			}
		}
		if !strings.Contains(res[1].Detail, "id 42") ||
			!strings.Contains(res[1].Detail, "runs-on: runpool-standard") {
			t.Errorf("existing set reads %q", res[1].Detail)
		}
		// A set that does not exist yet is not a finding, and the label is
		// what the operator came for either way.
		if !strings.Contains(res[2].Detail, "not created yet") ||
			!strings.Contains(res[2].Detail, "runs-on: runpool-heavy") {
			t.Errorf("absent set reads %q", res[2].Detail)
		}
	})

	t.Run("the provider refuses the scale set read", func(t *testing.T) {
		res := run(&fakeProbe{id: 7, setsErr: errors.New("403 Forbidden")})
		if len(res) != 3 {
			t.Fatalf("results = %+v; want the credential plus one per tier", res)
		}
		if res[0].Status != Pass {
			t.Errorf("the runner group check should still pass: %+v", res[0])
		}
		for _, r := range res[1:] {
			if r.Status != Fail || !strings.Contains(r.Detail, "403") {
				t.Errorf("%s = %+v; want the refusal reported", r.Name, r)
			}
		}
	})
}

// TestCheckCredentialsNamesTheIdentity: a permission answer is about an
// identity, and a deployment can now authenticate as two different kinds.
// An operator reading "the runner group is unreachable" needs to know
// which identity was refused; nothing about the secret itself helps them.
func TestCheckCredentialsNamesTheIdentity(t *testing.T) {
	for name, tc := range map[string]struct {
		credential config.Credential
		environ    func(string) string
		want       string
	}{
		"a personal access token": {
			credential: config.Credential{ID: "gh", TokenEnv: "TOKEN"},
			environ:    func(k string) string { return map[string]string{"TOKEN": "t0ken"}[k] },
			want:       "a personal access token",
		},
		"an app installation": {
			credential: config.Credential{
				ID: "gh", Type: config.CredentialTypeGitHubApp,
				ClientID: "Iv1.a", InstallationID: 987, PrivateKeyEnv: "KEY",
			},
			environ: func(k string) string {
				return map[string]string{"KEY": "-----BEGIN RSA PRIVATE KEY-----\nk\n-----END RSA PRIVATE KEY-----"}[k]
			},
			want: "app installation 987",
		},
	} {
		t.Run(name, func(t *testing.T) {
			var handed credential.Secret
			res := checkCredentials(t.Context(), Options{
				Config: &config.Config{
					Credentials: []config.Credential{tc.credential},
					Targets: []config.Target{
						{ID: "app", URL: "https://github.com/acme/app", CredentialID: "gh"},
					},
				},
				Environ: tc.environ,
				NewCredentialProbe: func(_ string, s credential.Secret) (CredentialProbe, error) {
					handed = s
					return &fakeProbe{id: 7}, nil
				},
			})
			if len(res) != 1 || res[0].Status != Pass {
				t.Fatalf("results = %+v; want one pass", res)
			}
			if !strings.Contains(res[0].Detail, tc.want) {
				t.Errorf("detail = %q, want it to name %q", res[0].Detail, tc.want)
			}
			// The probe is handed the credential whole: an app that
			// resolved to a token would authenticate as the wrong thing.
			if (tc.credential.Type == config.CredentialTypeGitHubApp) != (handed.App != nil) {
				t.Errorf("probe was handed %+v for a %q credential", handed, tc.credential.Type)
			}
		})
	}
}

// TestCheckCredentialsNamesTheHost: any https host is accepted by
// design, so where a credential travels has to be visible instead of
// assumed. The verdict names the host it proved, and a host the
// provider does not operate earns a warning beside the pass — the one
// moment the boundary is worth a second look is when the credential
// that crosses it has just been proven live.
func TestCheckCredentialsNamesTheHost(t *testing.T) {
	environ := func(k string) string {
		if k == "TOKEN" {
			return "t0ken"
		}
		return ""
	}
	cfgFor := func(url string) *config.Config {
		return &config.Config{
			Credentials: []config.Credential{{ID: "gh", TokenEnv: "TOKEN"}},
			Targets:     []config.Target{{ID: "app", URL: url, CredentialID: "gh"}},
		}
	}
	hosted := func(h string) bool { return h == "github.com" }

	res := checkCredentials(t.Context(), Options{
		Config: cfgFor("https://github.com/acme/app"), Environ: environ,
		NewCredentialProbe: func(string, credential.Secret) (CredentialProbe, error) {
			return &fakeProbe{id: 7}, nil
		},
		HostedDomain: hosted,
	})
	if len(res) != 1 || res[0].Status != Pass {
		t.Fatalf("hosted target = %+v; want one pass and no warning", res)
	}
	if !strings.Contains(res[0].Detail, "github.com") {
		t.Errorf("the verdict %q does not name the host it proved", res[0].Detail)
	}

	res = checkCredentials(t.Context(), Options{
		Config: cfgFor("https://ghes.internal/acme/app"), Environ: environ,
		NewCredentialProbe: func(string, credential.Secret) (CredentialProbe, error) {
			return &fakeProbe{id: 7}, nil
		},
		HostedDomain: hosted,
	})
	if len(res) != 2 || res[0].Status != Pass || res[1].Status != Warn {
		t.Fatalf("enterprise target = %+v; want the pass and the boundary warning", res)
	}
	if !strings.Contains(res[1].Detail, "ghes.internal") {
		t.Errorf("the warning %q does not name where the credential travels", res[1].Detail)
	}
}

// fakeDaemonInfo answers the one read checkCgroups makes.
type fakeDaemonInfo struct {
	info engine.HostInfo
	err  error
}

func (f fakeDaemonInfo) Info(context.Context) (engine.HostInfo, error) { return f.info, f.err }

// TestCheckCgroupsRefusesWhatItCannotEnforce: every branch here is a
// refusal an operator can hit, and each was unreachable while the check
// took the concrete client — a live daemon cannot present cgroup v1, an
// unknown driver or a missing controller on demand. A limit that
// silently does nothing is worse than one that fails, so each shape
// must close admission rather than warn.
func TestCheckCgroupsRefusesWhatItCannotEnforce(t *testing.T) {
	cfg := &config.Config{}
	good := engine.HostInfo{CgroupVersion: "2", CgroupDriver: "systemd", MemoryLimit: true, PidsLimit: true}

	fails := func(res []Result) bool {
		for _, r := range res {
			if r.Status == Fail {
				return true
			}
		}
		return false
	}

	if res := checkCgroups(t.Context(), nil, cfg); !fails(res) {
		t.Error("no daemon passed the cgroup check")
	}
	if res := checkCgroups(t.Context(), fakeDaemonInfo{err: errors.New("daemon down")}, cfg); !fails(res) {
		t.Error("an unanswerable daemon passed the cgroup check")
	}

	v1 := good
	v1.CgroupVersion = "1"
	if res := checkCgroups(t.Context(), fakeDaemonInfo{info: v1}, cfg); !fails(res) {
		t.Error("cgroup v1 passed; tier limits cannot be enforced on it")
	}

	odd := good
	odd.CgroupDriver = "openrc"
	if res := checkCgroups(t.Context(), fakeDaemonInfo{info: odd}, cfg); !fails(res) {
		t.Error("a driver this build cannot address passed; the capsule and gateway would run under separate budgets")
	}

	noMem := good
	noMem.MemoryLimit = false
	if res := checkCgroups(t.Context(), fakeDaemonInfo{info: noMem}, cfg); !fails(res) {
		t.Error("a host without the memory controller passed; the tier envelope would be advisory")
	}

	if res := checkCgroups(t.Context(), fakeDaemonInfo{info: good}, cfg); fails(res) {
		t.Errorf("a conforming host failed: %+v", res)
	}
}

// TestDoctorRunsWithoutADaemon is the panic guard, and it exists at the
// level of Run rather than of any one check.
//
// A nil *docker.Client converted into an interface is a non-nil value
// holding nothing, so every `d == nil` guard inside passes and the next
// line calls a method on nothing. That is exactly the state doctor is
// for — an operator whose daemon is not reachable runs it to be told so
// — and the panic took the whole report with it, including the checks
// that had already succeeded and would have said why.
func TestDoctorRunsWithoutADaemon(t *testing.T) {
	cfg := &config.Config{}
	config.ApplyDefaults(cfg)

	report := Run(t.Context(), Options{Config: cfg, StateDir: t.TempDir()})

	if len(report.Results) == 0 {
		t.Fatal("the report is empty; every check was lost with the daemon")
	}
	if report.OK() {
		t.Error("a host with no reachable daemon reported healthy")
	}
	var named []string
	for _, r := range report.Results {
		named = append(named, r.Name)
	}
	for _, want := range []string{"container engine", "isolated bridge"} {
		if !slices.Contains(named, want) {
			t.Errorf("no %q result in %v; the check never ran", want, named)
		}
	}
}

// TestCheckDaemonFailsClosed covers the three refusals that stop a host
// from serving. None is reachable from a live daemon — it cannot be
// asked to be absent, rootless, or older than the floor — so they were
// untested while the check took the concrete client.
func TestCheckDaemonFailsClosed(t *testing.T) {
	current := fmt.Sprintf("%d.0.0", platform.MinimumEngineMajor)
	tooOld := fmt.Sprintf("%d.9.9", platform.MinimumEngineMajor-1)

	for name, tc := range map[string]struct {
		d       daemonInfo
		wantOK  bool
		wantFix string
	}{
		"no daemon at all": {nil, false, "mount the daemon socket"},
		"the daemon cannot be read": {
			fakeDaemonInfo{err: errors.New("socket refused")}, false, "check the socket mount"},
		"rootless": {
			fakeDaemonInfo{info: engine.HostInfo{Rootless: true, ServerVersion: current}},
			false, "requires rootful Docker"},
		"below the engine floor": {
			fakeDaemonInfo{info: engine.HostInfo{ServerVersion: tooOld}}, false, "upgrade to Docker Engine"},
		"a daemon that serves": {
			fakeDaemonInfo{info: engine.HostInfo{ServerVersion: current}}, true, ""},
	} {
		t.Run(name, func(t *testing.T) {
			got := checkDaemon(t.Context(), tc.d)
			if (got.Status != Fail) != tc.wantOK {
				t.Fatalf("status = %v; want ok=%v (detail %q)", got.Status, tc.wantOK, got.Detail)
			}
			if tc.wantFix != "" && !strings.Contains(got.Fix+got.Detail, tc.wantFix) {
				t.Errorf("fix/detail = %q / %q; want it to mention %q", got.Fix, got.Detail, tc.wantFix)
			}
		})
	}
}
