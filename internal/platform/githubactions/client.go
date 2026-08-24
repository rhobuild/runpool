// Package githubactions is the GitHub Actions provider adapter: it
// wraps the actions/scaleset upstream API into typed scale-set
// operations with explicit not-found results, and message sessions that
// normalize the broker's two admission shapes into one. Everything
// GitHub-specific — scale sets, runner groups, JIT runners, request ids
// — lives here and in its contract tests; the rest of the controller
// sees provider-neutral assignments. Capacity policy belongs to the
// allocator; this package only announces what it is told.
package githubactions

import (
	"context"
	"fmt"
	"strings"

	"github.com/actions/scaleset"

	"github.com/rhobuild/runpool/internal/credential"
)

// IsHostedDomain reports whether GitHub itself operates the host: the
// public site or a data-residency domain. It mirrors the upstream
// client's own classification — github.com, www.github.com,
// github.localhost and any subdomain of ghe.com resolve to the hosted
// API endpoints, and every other host is addressed as an Enterprise
// Server. The predicate exists for visibility, not admission: a
// credential configured for an unrecognised host still travels there,
// and the startup log and the doctor are the surfaces that say so.
func IsHostedDomain(host string) bool {
	host = strings.ToLower(host)
	switch host {
	case "github.com", "www.github.com", "github.localhost":
		return true
	}
	return strings.HasSuffix(host, ".ghe.com")
}

type ClientConfig struct {
	// ConfigURL is the canonical target URL, at any scope a target
	// takes: repository, organization or enterprise.
	ConfigURL string
	// Credential is the resolved secret this client authenticates with. A
	// token authenticates as the person who minted it; an App installation
	// authenticates as itself, and the upstream client mints and refreshes
	// its installation token from the key.
	//
	// The domain's own type rather than one of this package's: every
	// caller that builds a client has one of these already, and a shape
	// declared here would be a translation each of them had to remember.
	Credential credential.Secret
	// Version identifies this build in GitHub's client telemetry.
	Version string
}

// Client wraps one upstream client for one GitHub target.
type Client struct {
	c *scaleset.Client
}

func NewClient(cfg ClientConfig) (*Client, error) {
	info := scaleset.SystemInfo{
		System:    "runpool",
		Version:   cfg.Version,
		Subsystem: "controller",
	}
	var c *scaleset.Client
	var err error
	if app := cfg.Credential.App; app != nil {
		// The installation token's whole lifecycle — the App JWT, the
		// exchange, and the refresh before expiry — belongs to the
		// upstream client. Reimplementing any of it here would be a
		// second answer to a question that already has one.
		auth := scaleset.GitHubAppAuth{
			ClientID:       app.ClientID,
			InstallationID: app.InstallationID,
			PrivateKey:     app.PrivateKey,
		}
		// The upstream constructor validates the config URL and nothing
		// about the credential, so an incomplete App would build a client
		// and fail on its first call with whatever the provider says about
		// a request it could not sign. Its own rule is exported; using it
		// here turns that into a refusal at startup.
		if err := auth.Validate(); err != nil {
			return nil, fmt.Errorf("github app credential: %w", err)
		}
		c, err = scaleset.NewClientWithGitHubApp(scaleset.ClientWithGitHubAppConfig{
			GitHubConfigURL: cfg.ConfigURL,
			GitHubAppAuth:   auth,
			SystemInfo:      info,
		})
	} else {
		// Symmetric with the App validation above. An empty token builds
		// a client that authenticates as nobody: the upstream constructor
		// accepts it, and the failure arrives later as the provider's
		// answer to an anonymous request — an authorization error about
		// the target, when the fault is a credential that resolved to
		// nothing.
		if cfg.Credential.Token == "" {
			return nil, fmt.Errorf("personal access token credential: the token is empty")
		}
		c, err = scaleset.NewClientWithPersonalAccessToken(scaleset.NewClientWithPersonalAccessTokenConfig{
			GitHubConfigURL:     cfg.ConfigURL,
			PersonalAccessToken: cfg.Credential.Token,
			SystemInfo:          info,
		})
	}
	if err != nil {
		return nil, err
	}
	return &Client{c: c}, nil
}

// ScaleSet is one upstream scale set this instance created or adopted.
type ScaleSet struct {
	ID        int
	Name      string
	GroupID   int
	GroupName string
	// Adopted reports that the set existed before this call — the
	// restart path, where creating a duplicate would strand the
	// original and its queued work.
	Adopted bool
	// DisableUpdate reports whether the set forbids runner self-update,
	// as the provider holds it - read back on adoption, and from the
	// create response on creation, so the caller always sees what the
	// provider recorded rather than what was asked for.
	DisableUpdate bool
}

// ScaleSetByName returns the current remote scale set without creating one.
// The boolean distinguishes an absent set from a failed lookup.
func (c *Client) ScaleSetByName(ctx context.Context, groupName, name string) (ScaleSet, bool, error) {
	if groupName == "" {
		groupName = scaleset.DefaultRunnerGroup
	}
	group, err := c.c.GetRunnerGroupByName(ctx, groupName)
	if err != nil {
		return ScaleSet{}, false, fmt.Errorf("resolve runner group %q: %w", groupName, err)
	}
	set, err := c.c.GetRunnerScaleSet(ctx, group.ID, name)
	if err != nil {
		return ScaleSet{}, false, fmt.Errorf("get scale set %q: %w", name, err)
	}
	if set == nil {
		return ScaleSet{Name: name, GroupID: group.ID, GroupName: group.Name}, false, nil
	}
	return ScaleSet{
		ID: set.ID, Name: name, GroupID: group.ID, GroupName: group.Name,
		DisableUpdate: set.RunnerSetting.DisableUpdate,
	}, true, nil
}

// RunnerGroupID resolves a runner group by name (empty means Default).
// It creates nothing, so the doctor can prove a credential reaches its
// target before any scale set exists.
func (c *Client) RunnerGroupID(ctx context.Context, groupName string) (int, error) {
	if groupName == "" {
		groupName = scaleset.DefaultRunnerGroup
	}
	group, err := c.c.GetRunnerGroupByName(ctx, groupName)
	if err != nil {
		return 0, err
	}
	return group.ID, nil
}

// EnsureScaleSet resolves the runner group (empty means Default) and
// returns the scale set for this binding, creating it when absent.
//
// knownID is the GitHub id this instance previously recorded for the
// binding, or zero when it has never created one. A set that already
// exists under the requested name is adopted only when its id matches
// that record: name equality is not ownership, and adopting a
// stranger's set would route work through it and later delete it.
// intended says an earlier pass recorded that it was about to create
// this exact name. It is what separates a set this instance created and
// failed to write down from a set that was simply already there: without
// it the first is indistinguishable from the second and the binding can
// never serve again.
//
// recordIntent writes that record, and is called only once the name is
// known to be free. Recording it any earlier would have a pass that is
// about to be refused leave behind the very evidence that tells a
// refusal from a crash, and the next pass would adopt the stranger this
// one declined.
func (c *Client) EnsureScaleSet(ctx context.Context, groupName, name string, knownID int, intended bool, recordIntent func() error) (ScaleSet, error) {
	existing, found, err := c.ScaleSetByName(ctx, groupName, name)
	if err != nil {
		return ScaleSet{}, err
	}
	if found {
		if knownID == 0 && !intended {
			return ScaleSet{}, fmt.Errorf(
				"scale set %q already exists in runner group %q (id %d) and this instance has no record of creating it; "+
					"choose another scaleSetName or remove the existing set",
				name, existing.GroupName, existing.ID)
		}
		if knownID != 0 && existing.ID != knownID {
			return ScaleSet{}, fmt.Errorf(
				"scale set %q in runner group %q has id %d but this instance recorded id %d; refusing to adopt a different set",
				name, existing.GroupName, existing.ID, knownID)
		}
		// The setting below is a property of the set, not of creating
		// one, so adoption asserts it too: a set this instance created
		// under an earlier build carries whatever that build asked for,
		// and every start after the first takes this branch. Only when it
		// is wrong, though — the read above already carries the answer,
		// and an unconditional write would put every restart's startup at
		// the mercy of one PATCH the healthy path never needed.
		if !existing.DisableUpdate {
			updated, err := c.c.UpdateRunnerScaleSet(ctx, existing.ID, &scaleset.RunnerScaleSet{
				RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true},
			})
			if err != nil {
				return ScaleSet{}, fmt.Errorf("forbid runner self-update on scale set %q: %w", name, err)
			}
			// The provider's answer is the only in-band evidence the
			// setting took; this endpoint is an undocumented preview, and
			// a 200 that ignored the field would otherwise read as done.
			if !updated.RunnerSetting.DisableUpdate {
				return ScaleSet{}, fmt.Errorf(
					"scale set %q still permits runner self-update after the update was accepted", name)
			}
			existing.DisableUpdate = true
		}
		existing.Adopted = true
		return existing, nil
	}
	// The name is free, so this is the moment the intention becomes true:
	// written down before the set exists, and only for a name nothing else
	// holds. The create below is the step that can be lost, and this is
	// what makes losing it recoverable.
	if err := recordIntent(); err != nil {
		return ScaleSet{}, fmt.Errorf("record the intention to create scale set %q: %w", name, err)
	}
	// DisableUpdate is what keeps the runner inside the image it was
	// built from. Left unset, the scale set tells a runner to upgrade
	// itself before taking work — which either replaces the binary the
	// capsule image pins by digest, or, under the restricted network
	// profile, is refused by the egress policy and fails the runner
	// before the job begins. Neither is a thing to discover at runtime.
	created, err := c.c.CreateRunnerScaleSet(ctx, &scaleset.RunnerScaleSet{
		Name:          name,
		RunnerGroupID: existing.GroupID,
		RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true},
	})
	if err != nil {
		return ScaleSet{}, fmt.Errorf("create scale set %q: %w", name, err)
	}
	// The same proof the adoption branch demands, for the same reason:
	// this is an undocumented preview endpoint, and a 200 that ignored
	// the field would run this instance's whole first lifetime with
	// self-update permitted - the one thing the setting exists to stop.
	if !created.RunnerSetting.DisableUpdate {
		return ScaleSet{}, fmt.Errorf(
			"scale set %q was created permitting runner self-update despite asking otherwise", name)
	}
	return ScaleSet{
		ID: created.ID, Name: name, GroupID: existing.GroupID, GroupName: existing.GroupName,
		DisableUpdate: true,
	}, nil
}

func (c *Client) DeleteScaleSet(ctx context.Context, id int) error {
	return c.c.DeleteRunnerScaleSet(ctx, id)
}

// JITConfig registers an ephemeral runner that has not started yet.
// Encoded is a one-shot secret: hand it to the runner process and drop
// it — it is never logged or persisted.
type JITConfig struct {
	RunnerID   int
	RunnerName string
	Encoded    string
}

func (c *Client) GenerateJITConfig(ctx context.Context, scaleSetID int, runnerName, workFolder string) (JITConfig, error) {
	jit, err := c.c.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{
		Name:       runnerName,
		WorkFolder: workFolder,
	}, scaleSetID)
	if err != nil {
		return JITConfig{}, fmt.Errorf("generate jit config for %q: %w", runnerName, err)
	}
	if jit.Runner == nil || jit.EncodedJITConfig == "" {
		return JITConfig{}, fmt.Errorf("jit config for %q came back incomplete", runnerName)
	}
	return JITConfig{RunnerID: jit.Runner.ID, RunnerName: jit.Runner.Name, Encoded: jit.EncodedJITConfig}, nil
}

type RunnerRef struct {
	ID   int
	Name string
}

// RunnerByName reports (ref, true) or (zero, false, nil): upstream
// encodes not-found as (nil, nil), and pretending that is an error —
// or worse, dereferencing it — is how cleanup code breaks.
func (c *Client) RunnerByName(ctx context.Context, name string) (RunnerRef, bool, error) {
	r, err := c.c.GetRunnerByName(ctx, name)
	if err != nil {
		return RunnerRef{}, false, err
	}
	if r == nil {
		return RunnerRef{}, false, nil
	}
	return RunnerRef{ID: r.ID, Name: r.Name}, true, nil
}

// The two refusals a deregistration can meet that mean different things.
// They are re-exported rather than left to the caller to match against
// the provider SDK, so the vocabulary stops at this adapter like every
// other provider concept does.
var (
	// ErrRunnerNotFound: the registration is already gone. Cleanup got
	// what it wanted.
	ErrRunnerNotFound = scaleset.RunnerNotFoundError
	// ErrJobStillRunning: the provider still considers this runner busy
	// and refuses to remove it. The registration outlives the capsule.
	ErrJobStillRunning = scaleset.JobStillRunningError
)

// RemoveRunner deregisters a runner, including one that never started —
// the failure-cleanup path for generated-but-unlaunched JIT runners.
//
// The error is wrapped rather than returned bare so errors.Is reaches the
// sentinels above: "already gone" and "the provider says it is busy" are
// opposite outcomes, and a caller that cannot tell them apart logs the
// same line for a successful cleanup and for a leaked registration.
func (c *Client) RemoveRunner(ctx context.Context, id int) error {
	return wrapRemoveRunner(id, c.c.RemoveRunner(ctx, int64(id)))
}

// wrapRemoveRunner adds the runner to the provider's answer without
// flattening it: the recovery path switches on the sentinels inside, and
// a wrap that dropped them would log a leaked registration as an
// ordinary cleanup failure. Named so the wrap itself is what the test
// holds -- constructed in the test, the same assertion proved only the
// standard library.
func wrapRemoveRunner(id int, err error) error {
	if err != nil {
		return fmt.Errorf("remove runner %d: %w", id, err)
	}
	return nil
}
