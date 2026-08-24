package config

import (
	"fmt"
	"math"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/rhobuild/runpool/internal/egress"
)

// FieldError locates one defect by its configuration path.
type FieldError struct {
	Path    string
	Message string
}

// ValidationError aggregates every defect found so the operator fixes the
// configuration in one round trip instead of one error per restart.
type ValidationError struct {
	Fields []FieldError
}

func (e *ValidationError) Error() string {
	var b strings.Builder
	b.WriteString("invalid configuration:")
	for _, f := range e.Fields {
		fmt.Fprintf(&b, "\n  %s: %s", f.Path, f.Message)
	}
	return b.String()
}

type validator struct {
	fields []FieldError
}

func (v *validator) errf(path, format string, args ...any) {
	v.fields = append(v.fields, FieldError{Path: path, Message: fmt.Sprintf(format, args...)})
}

// Validate checks the defaulted configuration against the V1 rules. Both
// Quick Start and file mode pass through here; there is one source of
// truth for what a valid configuration is.
//
// The host-reserve versus scheduling capacity rule needs live host facts
// and therefore belongs to the doctor, not to static validation.
func Validate(c *Config) error {
	v := &validator{}

	if c.APIVersion != APIVersion {
		v.errf("apiVersion", "must be %q", APIVersion)
	}
	if c.Kind != Kind {
		v.errf("kind", "must be %q", Kind)
	}
	if !slugRe.MatchString(c.Instance.Name) {
		v.errf("instance.name", "must be a lowercase slug")
	}
	if c.Host.Topology != HostTopologySharedDaemon && c.Host.Topology != HostTopologyDedicatedDaemon {
		v.errf("host.topology", "must be %q or %q", HostTopologySharedDaemon, HostTopologyDedicatedDaemon)
	}

	tiers := make(map[string]bool, len(c.Tiers))
	tiersUsed := make(map[string]bool, len(c.Tiers))
	totalParallelism := 0
	if len(c.Tiers) == 0 {
		v.errf("tiers", "at least one tier is required")
	}
	for i, t := range c.Tiers {
		path := fmt.Sprintf("tiers[%d]", i)
		if !slugRe.MatchString(t.ID) {
			v.errf(path+".id", "must be a lowercase slug")
		} else if tiers[t.ID] {
			v.errf(path+".id", "duplicate tier id %q", t.ID)
		}
		tiers[t.ID] = true
		if t.Parallelism < 1 {
			v.errf(path+".parallelism", "must be >= 1")
		} else if t.Parallelism > MaxParallelism {
			v.errf(path+".parallelism", "must be <= %d", MaxParallelism)
		} else {
			totalParallelism += t.Parallelism
		}
		if t.JobTimeout != nil && (*t.JobTimeout < MinJobTimeout || *t.JobTimeout > MaxJobTimeout) {
			v.errf(path+".jobTimeout", "must be between %s and %s; it is the wait for a capsule that stopped reporting, not the job's own limit",
				time.Duration(MinJobTimeout), time.Duration(MaxJobTimeout))
		}
		if t.CapsuleImage != "" && !IsDigestQualifiedImage(t.CapsuleImage) {
			v.errf(path+".capsuleImage",
				"must be digest-qualified (name@sha256:...); a tag can move under a running controller")
		}
		if t.Resources.CPU <= 0 {
			v.errf(path+".resources.cpu", "must be > 0")
		}
		if t.Resources.Memory <= 0 {
			v.errf(path+".resources.memory", "must be > 0")
		}
		if t.Resources.Swap < 0 {
			v.errf(path+".resources.swap", "must be >= 0B")
		} else if int64(t.Resources.Memory) > math.MaxInt64-int64(t.Resources.Swap) {
			v.errf(path+".resources.swap", "memory plus swap exceeds the runtime limit")
		}
		if t.Resources.PIDs < 1 {
			v.errf(path+".resources.pids", "must be >= 1")
		}
		// Under the restricted profile a lease's envelope covers its
		// capsule and its egress gateway together, so a tier must be
		// large enough to hold both. Rejecting it here is what keeps
		// the split from silently handing the capsule a negative or
		// unusable share.
		if c.Network.Profile != NetworkProfileUnsafeOpen {
			if int64(t.Resources.Memory) < GatewayReserveMemory+MinCapsuleMemory {
				v.errf(path+".resources.memory",
					"must be at least %s: the egress gateway reserves %s of the lease envelope and the capsule needs the rest",
					ByteSize(GatewayReserveMemory+MinCapsuleMemory), ByteSize(GatewayReserveMemory))
			}
			if int64(t.Resources.CPU) < GatewayReserveCPUs+MinCapsuleCPUs {
				v.errf(path+".resources.cpu",
					"must be at least %.1f: the egress gateway reserves %.1f of the lease envelope and the capsule needs the rest",
					float64(GatewayReserveCPUs+MinCapsuleCPUs)/1e9, float64(GatewayReserveCPUs)/1e9)
			}
			if t.Resources.PIDs < GatewayReservePIDs+MinCapsulePIDs {
				v.errf(path+".resources.pids",
					"must be at least %d: the egress gateway reserves %d of the lease envelope and the capsule needs the rest",
					GatewayReservePIDs+MinCapsulePIDs, GatewayReservePIDs)
			}
		}
	}
	if c.Scheduling.Parallelism != nil {
		switch {
		case *c.Scheduling.Parallelism < 1:
			v.errf("scheduling.parallelism", "must be >= 1 when set; omit the field to keep tiers independent")
		case *c.Scheduling.Parallelism > MaxParallelism:
			v.errf("scheduling.parallelism", "must be <= %d", MaxParallelism)
		case *c.Scheduling.Parallelism > totalParallelism:
			v.errf("scheduling.parallelism", "must not exceed aggregate tier parallelism %d", totalParallelism)
		}
	}

	credentials := make(map[string]bool, len(c.Credentials))
	credentialsUsed := make(map[string]bool, len(c.Credentials))
	if len(c.Credentials) == 0 {
		v.errf("credentials", "at least one credential is required")
	}
	for i, cr := range c.Credentials {
		path := fmt.Sprintf("credentials[%d]", i)
		if !slugRe.MatchString(cr.ID) {
			v.errf(path+".id", "must be a lowercase slug")
		} else if credentials[cr.ID] {
			v.errf(path+".id", "duplicate credential id %q", cr.ID)
		}
		credentials[cr.ID] = true
		switch cr.Type {
		case CredentialTypeToken:
			v.tokenCredential(path, cr)
		case CredentialTypeGitHubApp:
			v.appCredential(path, cr)
		default:
			v.errf(path+".type", "must be %q or %q", CredentialTypeToken, CredentialTypeGitHubApp)
		}
	}

	if b := c.Scheduling.RetryBudget; b != 0 && (b < MinRetryBudget || b > MaxRetryBudget) {
		v.errf("scheduling.retryBudget", "must be between %d and %d",
			MinRetryBudget, MaxRetryBudget)
	}

	if len(c.Targets) == 0 {
		v.errf("targets", "at least one target is required")
	}
	targetIDs := make(map[string]bool, len(c.Targets))
	targetURLs := make(map[string]bool, len(c.Targets))
	for i, t := range c.Targets {
		path := fmt.Sprintf("targets[%d]", i)
		if !slugRe.MatchString(t.ID) {
			v.errf(path+".id", "must be a lowercase slug")
		} else if targetIDs[t.ID] {
			v.errf(path+".id", "duplicate target id %q", t.ID)
		}
		targetIDs[t.ID] = true

		ref, err := ParseTargetURL(t.URL)
		if err != nil {
			v.errf(path+".url", "%v", err)
		} else {
			// Keyed case-insensitively: GitHub logins and repository
			// names are, so two targets differing only in case name one
			// scope. Accepted as distinct they each take a binding key of
			// their own, and the scale-set name check below is written on
			// the premise that target URLs are unique — so both would
			// carry the same default name and collide on one remote set.
			scope := strings.ToLower(ref.CanonicalURL)
			if targetURLs[scope] {
				v.errf(path+".url", "duplicate target %s: bind additional tiers on the existing target instead", ref.CanonicalURL)
			}
			targetURLs[scope] = true
			// A JIT runner is not bound to the job whose demand message
			// triggered it — measured live, not assumed — so only a
			// repository-scoped scale set may mount a repository cache.
			if ref.Scope != ScopeRepository && t.Cache.Enabled {
				v.errf(path+".cache.enabled", "persistent cache requires a repository-scoped target; a runner that is not bound to one repository could execute another's job against its cache")
			}
			// Enterprises have runner groups for the same reason
			// organizations do: a scale set that is not in one shares a
			// pool with runners this instance does not own.
			grouped := ref.Scope == ScopeOrganization || ref.Scope == ScopeEnterprise
			if !grouped && t.RunnerGroup != "" {
				v.errf(path+".runnerGroup", "runner groups apply to organization and enterprise targets only")
			}
			if c.Host.Topology == HostTopologySharedDaemon && grouped && t.RunnerGroup == "" {
				v.errf(path+".runnerGroup", "is required for %s targets on a shared daemon; isolate Runpool runners from unrelated self-hosted runners", ref.Scope)
			}
		}

		if t.CredentialID == "" {
			v.errf(path+".credential", "is required")
		} else if !credentials[t.CredentialID] {
			v.errf(path+".credential", "unknown credential %q", t.CredentialID)
		}
		credentialsUsed[t.CredentialID] = true

		if t.Cache.Enabled && !slugRe.MatchString(t.Cache.Generation) {
			v.errf(path+".cache.generation", "must be a lowercase slug")
		}
		if t.RunnerGroup != "" && !slugRe.MatchString(t.RunnerGroup) {
			v.errf(path+".runnerGroup", "must be a lowercase slug")
		}

		if len(t.Tiers) == 0 {
			v.errf(path+".tiers", "at least one tier binding is required")
		}
		names := make(map[string]bool, len(t.Tiers))
		refs := make(map[string]bool, len(t.Tiers))
		for j, b := range t.Tiers {
			bpath := fmt.Sprintf("%s.tiers[%d]", path, j)
			if !tiers[b.TierID] {
				v.errf(bpath+".tier", "unknown tier %q", b.TierID)
			} else if refs[b.TierID] {
				v.errf(bpath+".tier", "duplicate binding for tier %q", b.TierID)
			}
			refs[b.TierID] = true
			tiersUsed[b.TierID] = true
			if !slugRe.MatchString(b.ScaleSetName) {
				v.errf(bpath+".scaleSetName", "must be a lowercase slug")
			} else if names[b.ScaleSetName] {
				// Target URLs are unique, so a name collision within one
				// GitHub scope can only happen between bindings of the
				// same target.
				v.errf(bpath+".scaleSetName", "duplicate scale-set name %q in the same target scope", b.ScaleSetName)
			}
			names[b.ScaleSetName] = true
		}
	}

	// More bindings than tier parallelism is legal: the bindings share
	// capacity credits, and a binding with nothing to run holds none. It
	// stays visible through the tier's rotating discovery credit rather
	// than through a reservation of its own.

	for id := range tiers {
		if !tiersUsed[id] {
			v.errf("tiers", "tier %q is not referenced by any target", id)
		}
	}
	for id := range credentials {
		if !credentialsUsed[id] {
			v.errf("credentials", "credential %q is not referenced by any target", id)
		}
	}

	r := c.Host.Reserve
	if r.CPU < 0 || r.Memory < 0 || r.Swap < 0 || r.FreeDisk < 0 {
		v.errf("host.reserve", "cpu, memory, swap and freeDisk must not be negative")
	}
	if c.Host.Topology == HostTopologySharedDaemon && (r.CPU <= 0 || r.Memory <= 0 || r.FreeDisk <= 0) {
		v.errf("host.reserve", "cpu, memory and freeDisk must all be explicit and greater than zero on a shared daemon")
	}
	if c.Host.Topology == HostTopologySharedDaemon && c.Network.Profile == NetworkProfileUnsafeOpen {
		v.errf("network.profile", "%q is not supported on a shared daemon; use %q or move Runpool to a dedicated daemon",
			NetworkProfileUnsafeOpen, NetworkProfilePublicInternetOnly)
	}

	if c.Cache.Storage.Mode != CacheStorageModeVolume {
		v.errf("cache.storage.mode", "must be %q; the quota-backed bind mode arrives with the advanced storage feature", CacheStorageModeVolume)
	}
	g := c.Cache.Global
	if g.LowWatermarkPercent < 1 || g.HighWatermarkPercent > 99 || g.LowWatermarkPercent >= g.HighWatermarkPercent {
		v.errf("cache.global", "watermarks must satisfy 1 <= low < high <= 99")
	}
	if g.HardEmergencyFreeBytes >= g.SoftEmergencyFreeBytes {
		v.errf("cache.global.hardEmergencyFreeBytes", "must be below softEmergencyFreeBytes")
	}
	if c.Cache.Defaults.RepositoryMaxBytes > g.MaxManagedBytes {
		v.errf("cache.defaults.repositoryMaxBytes", "must not exceed cache.global.maxManagedBytes")
	}
	if c.Cache.Defaults.UnusedTTL <= 0 {
		v.errf("cache.defaults.unusedTTL", "must be > 0")
	}
	// Zero is legal here and means keep forever, so the `> 0` rule the
	// cache TTL uses does not apply. What is refused is a negative value
	// and a window so short it would delete work an operator is still
	// reading.
	if h := c.Retention.LeaseHistory; h != nil {
		switch {
		case *h < 0:
			v.errf("retention.leaseHistory", "must not be negative")
		case *h > 0 && *h < MinLeaseHistory:
			v.errf("retention.leaseHistory", "must be at least %s, or 0 to keep every lease record",
				MinLeaseHistory)
		}
	}

	// There is no metrics endpoint yet: budget, credits and pressure
	// are reported through status and the structured log. Accepting
	// `enabled: true` would have an operator wire an alert to a port
	// nothing listens on.
	if c.Observability.Metrics.Enabled {
		v.errf("observability.metrics.enabled", "must be false; no metrics endpoint exists yet — capacity, credits and disk pressure are reported by `runpool status` and the structured log")
	}

	if f := c.Observability.Log.Format; f != LogFormatJSON && f != LogFormatText {
		v.errf("observability.log.format", "must be %q or %q", LogFormatJSON, LogFormatText)
	}
	if !slices.Contains(AllLogLevels, c.Observability.Log.Level) {
		v.errf("observability.log.level", "must be one of %s", joinVocabulary(AllLogLevels))
	}

	// The restricted profile sandboxes egress: no route out, and a
	// policy-enforcing relay for what is allowed. The other name is the
	// deployment that accepts open egress and must say so.
	if c.Network.Profile != NetworkProfilePublicInternetOnly && c.Network.Profile != NetworkProfileUnsafeOpen {
		v.errf("network.profile", "must be %q (restricted) or %q",
			NetworkProfilePublicInternetOnly, NetworkProfileUnsafeOpen)
	}
	// A capsule's network carries no IPv6 and its gateway denies the
	// protocol outright, so "disabled" is the only value that describes
	// what runs. Accepting "enforced" would promise parity that does
	// not exist.
	// The egress policy is rendered into an IPv4 ruleset, and an allow
	// prefix is a hole punched through the baseline deny. Three facts have
	// to be checked here: an IPv6 prefix passes CIDR parsing and then fails
	// at gateway boot pointing at iptables instead of at the config line;
	// a public or over-wide allow silently reopens what the restricted
	// profile exists to close; and an allow through a range no relay
	// reaches is a line that does nothing, while the ruleset still carries
	// its accept -- a firewall that agrees with the file and a gateway
	// that refuses every request, with nothing saying why.
	//
	// The policy holds the same two rules, because a gateway takes one
	// from its reload channel as well as from here. This names the
	// configuration field an operator has to fix.
	for i, p := range c.Network.AllowPrivateCIDRs {
		path := fmt.Sprintf("network.allowPrivateCIDRs[%d]", i)
		switch {
		case !p.Addr().Is4():
			// Not Unmap: the v4-in-v6 form unmaps to IPv4 and then never
			// matches an address at decision time, because a 128-bit
			// prefix contains no 32-bit one. It renders into the ruleset
			// all the same.
			v.errf(path, "must be IPv4: the capsule egress ruleset is IPv4 only, and an "+
				"address written in the v4-in-v6 form is not one the relay matches")
		case egress.WidensBaselineDeny(p.Prefix):
			v.errf(path, "%s is broader than a range the restricted profile withholds, "+
				"so allowing it would reopen that whole range; name the specific addresses instead", p)
		case egress.RefusedOutright(p.Prefix):
			v.errf(path, "%s names addresses no relay reaches, so it cannot take effect: "+
				"loopback is the gateway itself, and multicast, broadcast and the unspecified "+
				"address are not destinations a connection can have", p)
		case egress.ReopensLinkLocal(p.Prefix):
			v.errf(path, "%s reaches more of link-local than one address, which would hand a "+
				"job the range its instance keeps its own credentials in; name the address", p)
		}
	}
	for i, p := range c.Network.DenyCIDRs {
		if !p.Addr().Is4() {
			v.errf(fmt.Sprintf("network.denyCIDRs[%d]", i), "must be IPv4: the capsule egress ruleset is IPv4 only")
		}
	}
	if c.Network.IPv6 != IPv6Disabled {
		v.errf("network.ipv6", "must be %q; IPv6 parity for the capsule network is not implemented, and the sandbox denies the protocol", IPv6Disabled)
	}
	if c.Network.DNS.Mode != DNSModeGateway {
		v.errf("network.dns.mode", "must be %q", DNSModeGateway)
	}

	if len(v.fields) > 0 {
		return &ValidationError{Fields: v.fields}
	}
	return nil
}

// tokenCredential checks a credential that authenticates as a person. The
// fields of the other kind must be absent rather than ignored: a
// credential carrying both is a deployment that believes something about
// which one is in use, and only one of those beliefs is right.
func (v *validator) tokenCredential(path string, cr Credential) {
	v.secretRef(path, "tokenEnv", "tokenFile", cr.TokenEnv, cr.TokenFile)
	if cr.ClientID != "" || cr.InstallationID != 0 ||
		cr.PrivateKeyEnv != "" || cr.PrivateKeyFile != "" {
		v.errf(path, "clientID, installationID and the private key belong to a %q credential",
			CredentialTypeGitHubApp)
	}
}

// appCredential checks a credential that authenticates as an installation
// of a GitHub App. The client id and installation id are not secret; the
// key is, and it is referenced the same two ways a token is.
func (v *validator) appCredential(path string, cr Credential) {
	if cr.ClientID == "" {
		v.errf(path+".clientID", "is required for a %q credential", CredentialTypeGitHubApp)
	}
	if cr.InstallationID <= 0 {
		v.errf(path+".installationID", "must be > 0; it identifies the installation this deployment acts as")
	}
	v.secretRef(path, "privateKeyEnv", "privateKeyFile", cr.PrivateKeyEnv, cr.PrivateKeyFile)
	if cr.TokenEnv != "" || cr.TokenFile != "" {
		v.errf(path, "tokenEnv and tokenFile belong to a %q credential", CredentialTypeToken)
	}
}

// secretRef holds every env-xor-file secret reference to one rule:
// exactly one side set, a well-formed variable name, a reviewable file
// path. The field names arrive as parameters so the third credential
// type costs a call here rather than a third copy of the ladder — and
// so the two existing copies cannot drift apart again.
func (v *validator) secretRef(path, envField, fileField, envName, filePath string) {
	switch {
	case envName == "" && filePath == "":
		v.errf(path, "exactly one of %s or %s is required", envField, fileField)
	case envName != "" && filePath != "":
		v.errf(path, "%s and %s are mutually exclusive", envField, fileField)
	case envName != "" && !envNameRe.MatchString(envName):
		v.errf(path+"."+envField, "must be an environment variable name")
	default:
		v.secretPath(path+"."+fileField, filePath)
	}
}

// secretPath refuses a reference that names a different file depending on
// how the process was started. The controller's working directory is not
// part of the contract.
func (v *validator) secretPath(path, ref string) {
	switch {
	case ref == "":
	case !filepath.IsAbs(ref):
		v.errf(path, "must be an absolute path")
	case filepath.Clean(ref) != ref:
		v.errf(path, "must be a clean path; %q resolves to %q", ref, filepath.Clean(ref))
	}
}

// joinVocabulary renders a closed vocabulary for an error an operator
// reads. The values are named types, so they need one conversion each --
// and doing it here keeps every refusal message spelling the set the
// same way.
func joinVocabulary[T ~string](vs []T) string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = string(v)
	}
	return strings.Join(out, ", ")
}
