// Package doctor validates that a host can honour the capsule contract
// before Runpool admits any work: the daemon's version, rootful mode,
// cgroup v2 with the controllers the tier limits need, writable state
// storage, and the configured credentials and targets.
//
// Every check reports pass, warn or fail with an actionable message.
// A failed check closes admission — the design's rule that a host which
// cannot meet the contract must fail before creating scale sets, not
// midway through a job.
package doctor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/rhobuild/runpool/internal/capsule"
	"github.com/rhobuild/runpool/internal/config"
	"github.com/rhobuild/runpool/internal/credential"
	"github.com/rhobuild/runpool/internal/platform"
	"github.com/rhobuild/runpool/internal/platform/docker"
)

type Status string

const (
	Pass Status = "pass"
	Warn Status = "warn"
	Fail Status = "fail"
)

// Result is one check's verdict. Detail states what was observed; Fix,
// when set, says what the operator should do about it.
type Result struct {
	Name   string `json:"name"`
	Status Status `json:"status"`
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

// Report is a whole doctor run.
type Report struct {
	Results []Result `json:"results"`
}

// OK reports whether every check passed or warned — the admission gate.
func (r Report) OK() bool {
	for _, res := range r.Results {
		if res.Status == Fail {
			return false
		}
	}
	return true
}

func (r Report) String() string {
	var b strings.Builder
	for _, res := range r.Results {
		fmt.Fprintf(&b, "%-4s %-22s %s\n", res.Status, res.Name, res.Detail)
		if res.Fix != "" {
			fmt.Fprintf(&b, "     %-22s → %s\n", "", res.Fix)
		}
	}
	return b.String()
}

// Options are the paths and configuration a run checks against. Docker
// may be nil, in which case the daemon checks report the connection
// failure instead of panicking.
type Options struct {
	Config   *config.Config
	Docker   *docker.Client
	StateDir string
	Environ  func(string) string
	// NewCredentialProbe builds the probe for one target, from that
	// target's URL and resolved token. A nil factory skips the live
	// credential checks, which is what a caller wanting only local host
	// validation asks for — the presence of a way to reach the provider
	// is the switch, so there is no second flag to keep in step with it.
	//
	// It is a factory rather than a client because the provider is
	// reached per target: each has its own URL and credential.
	NewCredentialProbe func(configURL string, secret credential.Secret) (CredentialProbe, error)
}

// CredentialProbe proves one credential can reach one target's runner
// group — the whole provider surface this package needs. Declaring it
// here rather than importing an adapter is what keeps the doctor
// provider-neutral, the same shape as networkProbeClient below.
type CredentialProbe interface {
	RunnerGroupID(ctx context.Context, group string) (int, error)
	// ScaleSetID reports the provider's id for a scale set this instance
	// would serve, and whether it exists yet. Not existing is not a
	// finding: the controller creates it on its first serve.
	ScaleSetID(ctx context.Context, group, name string) (int, bool, error)
}

// Run executes every check and returns the report. It never mutates
// durable product state; the disposable probes it may create are labeled
// and removed.
func Run(ctx context.Context, opts Options) Report {
	var r Report
	add := func(res Result) { r.Results = append(r.Results, res) }

	add(checkPlatform())
	if opts.Config != nil {
		add(checkHostTopology(opts.Config))
	}
	add(checkDaemon(ctx, opts.Docker))
	add(checkIsolatedBridge(ctx, opts.Docker))
	for _, res := range checkCgroups(ctx, opts.Docker, opts.Config) {
		add(res)
	}
	// Cache lanes are daemon-side volumes now, so the daemon check above
	// covers their storage; only the state directory is the
	// controller's own filesystem concern.
	add(checkStorage("state directory", opts.StateDir))
	if opts.Config != nil {
		add(checkCapacity(opts.Config))
		add(checkPhysicalCapacity(ctx, opts.Config, opts.Docker))
		if opts.NewCredentialProbe != nil {
			for _, res := range checkCredentials(ctx, opts) {
				add(res)
			}
		}
	}
	return r
}

func checkHostTopology(cfg *config.Config) Result {
	switch cfg.Host.Topology {
	case config.HostTopologySharedDaemon:
		return Result{
			Name:   "host topology",
			Status: Warn,
			Detail: "shared Docker daemon; Runpool reservations and ownership filters protect coexistence, not daemon compromise",
			Fix:    "exclude Runpool-managed volumes from platform-wide prune and run only trusted private CI workflows on this host",
		}
	case config.HostTopologyDedicatedDaemon:
		return Result{"host topology", Pass, "dedicated Docker daemon", ""}
	default:
		return Result{"host topology", Fail, "not configured", "set host.topology explicitly"}
	}
}

func checkPlatform() Result {
	if runtime.GOOS != "linux" {
		return Result{"platform", Fail, runtime.GOOS + "/" + runtime.GOARCH + " cannot run Linux capsules",
			"run the controller on a Linux host"}
	}
	return Result{"platform", Pass, runtime.GOOS + "/" + runtime.GOARCH, ""}
}

func checkDaemon(ctx context.Context, d *docker.Client) Result {
	if d == nil {
		return Result{"docker daemon", Fail, "not connected",
			"mount the daemon socket at /var/run/docker.sock"}
	}
	info, err := d.Info(ctx)
	if err != nil {
		return Result{"docker daemon", Fail, err.Error(), "check the socket mount and daemon health"}
	}
	if info.Rootless {
		return Result{"docker daemon", Fail, "rootless mode (engine " + info.ServerVersion + ")",
			"V1 requires rootful Docker: privileged dind and cgroup limits differ under rootless"}
	}
	major := majorVersion(info.ServerVersion)
	detail := fmt.Sprintf("engine %s, api %s, %s", info.ServerVersion, info.APIVersion, info.Architecture)
	if major < platform.MinimumEngineMajor {
		return Result{"docker daemon", Fail, detail,
			fmt.Sprintf("upgrade to Docker Engine %d or newer", platform.MinimumEngineMajor)}
	}
	return Result{"docker daemon", Pass, detail, ""}
}

// checkIsolatedBridge proves the daemon actually provides the capability
// the restricted profile depends on, rather than inferring it from a
// version number. Engine 28 introduced the isolated bridge gateway mode,
// but a version string is a claim: a backport, a patched build, a future
// major that drops or renames the option, or a daemon whose network
// driver is unavailable will all announce a sufficient version and then
// refuse the network a capsule needs.
//
// The probe creates one disposable internal+isolated bridge, labeled as
// managed so any sweep can recognise it, and removes it immediately. It
// carries no containers and reserves nothing beyond a subnet for the
// moment it exists. Failing here is the point: a host that cannot build
// this network must be refused before it accepts work, not after a job
// has already been assigned to it.
type networkProbeClient interface {
	CreateNetwork(context.Context, docker.NetworkSpec) (string, error)
	RemoveNetwork(context.Context, string) error
}

func checkIsolatedBridge(ctx context.Context, d networkProbeClient) Result {
	const name = "isolated bridge"
	if d == nil {
		return Result{name, Fail, "daemon not connected", ""}
	}
	suffix, err := probeSuffix()
	if err != nil {
		return Result{name, Fail, "cannot generate a unique probe name: " + err.Error(), "check the host random source"}
	}
	probe := "runpool-doctor-" + suffix
	id, err := d.CreateNetwork(ctx, docker.NetworkSpec{
		Name:     probe,
		Internal: true,
		Isolated: true,
		Labels: map[string]string{
			docker.LabelManaged:  "true",
			docker.LabelInstance: "doctor",
			docker.LabelKind:     "network",
			docker.LabelRole:     "preflight-probe",
		},
	})
	if err != nil {
		cleanupErr := removeProbeNetwork(ctx, d, probe)
		if cleanupErr != nil {
			return Result{name, Fail,
				"network creation was ambiguous and cleanup failed: " + errors.Join(err, cleanupErr).Error(),
				"inspect and remove " + probe + " before retrying"}
		}
		return Result{name, Fail, "daemon refused an internal isolated bridge: " + err.Error(),
			"the restricted network profile needs the Engine 28 isolated gateway mode; " +
				"use a daemon that provides it, or accept unsafe-open-egress deliberately"}
	}
	if err := removeProbeNetwork(ctx, d, id); err != nil {
		return Result{name, Fail,
			"created, but the probe network could not be removed: " + err.Error(),
			"remove " + probe + " manually"}
	}
	return Result{name, Pass, "internal isolated bridge created and removed", ""}
}

func removeProbeNetwork(ctx context.Context, d networkProbeClient, id string) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	return d.RemoveNetwork(cleanupCtx, id)
}

// probeSuffix names one probe uniquely so two concurrent preflights do
// not collide on the name, and so a leaked probe is traceable to a run.
func probeSuffix() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func checkCgroups(ctx context.Context, d *docker.Client, cfg *config.Config) []Result {
	if d == nil {
		return []Result{{"cgroups", Fail, "daemon not connected", ""}}
	}
	info, err := d.Info(ctx)
	if err != nil {
		return []Result{{"cgroups", Fail, err.Error(), ""}}
	}
	var out []Result
	switch {
	case info.CgroupVersion != "2":
		out = append(out, Result{"cgroups", Fail, "version " + info.CgroupVersion,
			"V1 requires cgroup v2 for the tier CPU, memory and PID limits"})
	case !capsule.KnownCgroupDriver(info.CgroupDriver):
		// The parent cgroup's form is generated for the driver, and a
		// driver this build cannot write a parent for produces no parent
		// at all — so the capsule and its gateway would run under
		// separate budgets and the tier would stop being their sum. It is
		// silent at every later point, which is why it fails here.
		out = append(out, Result{"cgroups", Fail,
			"v2, driver " + info.CgroupDriver + ", which this build cannot address",
			"the tier envelope needs one parent cgroup per lease, and its form is written for systemd or cgroupfs"})
	default:
		out = append(out, Result{"cgroups", Pass, "v2, driver " + info.CgroupDriver, ""})
	}
	// Without these controllers the tier envelope is advisory, which the
	// resource contract does not allow.
	if !info.MemoryLimit || !info.PidsLimit {
		out = append(out, Result{"cgroup controllers", Fail,
			fmt.Sprintf("memory=%v pids=%v", info.MemoryLimit, info.PidsLimit),
			"enable the memory and pids controllers; tier limits cannot be enforced without them"})
	} else {
		out = append(out, Result{"cgroup controllers", Pass, "memory and pids enforceable", ""})
	}
	if configuredSwap(cfg) > 0 {
		if !info.SwapLimit {
			out = append(out, Result{"swap controller", Fail, "Docker cannot enforce memory swap limits",
				"enable swap accounting for the cgroup v2 memory controller or set every configured swap value to 0B"})
		} else {
			out = append(out, Result{"swap controller", Pass, "memory swap limits enforceable", ""})
		}
	}
	return out
}

// checkStorage proves the directory exists and is writable by actually
// writing, since a read-only mount is the failure this must catch.
func checkStorage(name, dir string) Result {
	if dir == "" {
		return Result{name, Fail, "not configured", ""}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Result{name, Fail, err.Error(), "mount a writable volume at " + dir}
	}
	probe, err := os.CreateTemp(dir, ".runpool-write-probe-*")
	if err != nil {
		return Result{name, Fail, "not writable: " + err.Error(), "mount " + dir + " read-write"}
	}
	probeName := probe.Name()
	if _, err := probe.WriteString("ok"); err != nil {
		_ = probe.Close()
		_ = os.Remove(probeName)
		return Result{name, Fail, "write probe failed: " + err.Error(), "check free space and filesystem health at " + dir}
	}
	if err := probe.Close(); err != nil {
		_ = os.Remove(probeName)
		return Result{name, Fail, "write probe close failed: " + err.Error(), "check filesystem health at " + dir}
	}
	if err := os.Remove(probeName); err != nil {
		return Result{name, Fail, "write probe could not be removed: " + err.Error(), "remove " + probeName + " and check directory permissions"}
	}
	return Result{name, Pass, dir + " writable", ""}
}

// checkCredentials resolves each target's credential and proves the token
// can reach the target's runner group — the permission failure operators
// hit most, surfaced before any scale set is created.
//
// It names no provider. The caller supplies the probe, which is what lets
// this package stay in the architecture test's core set alongside every
// other domain package.
func checkCredentials(ctx context.Context, opts Options) []Result {
	creds := map[string]config.Credential{}
	for _, c := range opts.Config.Credentials {
		creds[c.ID] = c
	}
	var out []Result
	for _, target := range opts.Config.Targets {
		name := "target " + target.ID
		ref, err := config.ParseTargetURL(target.URL)
		if err != nil {
			out = append(out, Result{name, Fail, err.Error(), ""})
			continue
		}
		secret, err := credential.Resolve(opts.Environ, creds[target.CredentialID])
		if err != nil {
			out = append(out, Result{name, Fail, err.Error(), "provide the credential the target references"})
			continue
		}
		probe, err := opts.NewCredentialProbe(ref.CanonicalURL, secret)
		if err != nil {
			out = append(out, Result{name, Fail, err.Error(), ""})
			continue
		}
		group := target.RunnerGroup
		if group == "" {
			group = "default"
		}
		if _, err := probe.RunnerGroupID(ctx, group); err != nil {
			out = append(out, Result{name, Fail, "cannot reach runner group " + group + ": " + err.Error(),
				"the credential needs administration on the target (repository) or self-hosted runners (organization)"})
			continue
		}
		out = append(out, Result{name, Pass,
			fmt.Sprintf("%s scope, runner group %s reachable, authenticated as %s",
				ref.Scope, group, describe(secret)), ""})
		out = append(out, scaleSetResults(ctx, probe, target, group)...)
	}
	return out
}

func majorVersion(v string) int {
	major, _, _ := strings.Cut(v, ".")
	n := 0
	for _, r := range major {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// scaleSetResults reports each scale set this target would serve, and the
// label a workflow reaches it by.
//
// The label is the reason this check exists. A scale set is named in
// configuration and matched by workflows, and nothing else an operator
// can run says the two are the same string — so a workflow that names it
// wrongly queues forever against an instance whose every other check
// passes.
func scaleSetResults(ctx context.Context, probe CredentialProbe, target config.Target, group string) []Result {
	var out []Result
	for _, tb := range target.Tiers {
		name := "scale set " + tb.ScaleSetName
		id, found, err := probe.ScaleSetID(ctx, group, tb.ScaleSetName)
		switch {
		case err != nil:
			out = append(out, Result{name, Fail, err.Error(),
				"the credential reached the runner group but not its scale sets"})
		case found:
			out = append(out, Result{name, Pass,
				fmt.Sprintf("id %d; workflows reach it with runs-on: %s", id, tb.ScaleSetName), ""})
		default:
			out = append(out, Result{name, Pass,
				fmt.Sprintf("not created yet; serve creates it, and workflows reach it with runs-on: %s",
					tb.ScaleSetName), ""})
		}
	}
	return out
}

// describe names what a credential authenticates as, without naming what
// it is. An operator debugging a permission answer needs to know which
// identity was refused; nothing about the value helps them.
func describe(secret credential.Secret) string {
	if secret.App != nil {
		return fmt.Sprintf("app installation %d", secret.App.InstallationID)
	}
	return "a personal access token"
}
