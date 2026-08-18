// Package config defines the Runpool configuration schema shared by the
// Quick Start environment translation and the advanced YAML file, applies
// product defaults, and validates the result into the canonical in-memory
// form every other subsystem consumes.
package config

import (
	"fmt"
	"math"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// APIVersion and Kind identify a configuration document. Unknown values are
// rejected so schema evolution stays explicit.
const (
	APIVersion = "runpool.rhobuild.com/v1"
	Kind       = "RunpoolConfig"
)

type Config struct {
	APIVersion    string        `yaml:"apiVersion"`
	Kind          string        `yaml:"kind"`
	Instance      Instance      `yaml:"instance"`
	Host          Host          `yaml:"host"`
	Scheduling    Scheduling    `yaml:"scheduling,omitempty"`
	Targets       []Target      `yaml:"targets"`
	Credentials   []Credential  `yaml:"credentials"`
	Tiers         []Tier        `yaml:"tiers"`
	Cache         Cache         `yaml:"cache"`
	Retention     Retention     `yaml:"retention"`
	Observability Observability `yaml:"observability"`
	Network       Network       `yaml:"network"`
}

// Retention is how long durable records outlive the work they describe.
type Retention struct {
	// LeaseHistory is how long the record of a finished lease is kept.
	// Zero keeps it forever; nil means unset and takes the default.
	//
	// It is a pointer because zero is a decision here, not an absence,
	// and defaulting keys on the zero value everywhere else in this
	// package — without the pointer, "keep forever" and "not configured"
	// would be the same value.
	//
	// This is the record of the host resources an attempt consumed, not
	// the attempt itself: what the work did is the attempt's evidence and
	// is never pruned.
	LeaseHistory *Duration `yaml:"leaseHistory"`
}

// Window is the retention window in force: how long a finished lease's
// record is kept, with zero meaning forever.
//
// It exists so the pointer is unwrapped in one place. A loaded
// configuration never carries nil — ApplyDefaults materialises it — so
// the fallback only serves a Retention built in memory, and callers that
// spelled that fallback themselves were writing a branch they could not
// reach and diverging from each other in the process.
func (r Retention) Window() time.Duration {
	if r.LeaseHistory == nil {
		return time.Duration(DefaultLeaseHistory)
	}
	return time.Duration(*r.LeaseHistory)
}

type Instance struct {
	Name string `yaml:"name"`
}

type Host struct {
	// Topology states who else relies on the Docker daemon. Shared-daemon
	// enables the coexistence contract used by platforms such as Dokploy;
	// dedicated-daemon gives Runpool exclusive operational ownership.
	Topology string  `yaml:"topology"`
	Reserve  Reserve `yaml:"reserve"`
}

// Reserve is capacity withheld from tier scheduling so the host, the
// controller and the platform keep breathing room.
type Reserve struct {
	CPU      CPUQuantity `yaml:"cpu"`
	Memory   ByteSize    `yaml:"memory"`
	Swap     ByteSize    `yaml:"swap"`
	FreeDisk ByteSize    `yaml:"freeDisk"`
}

// Scheduling contains instance-wide admission policy. A nil Parallelism
// leaves tiers independent; a value caps active leases and advertised
// provider capacity across every target and tier in the instance.
type Scheduling struct {
	Parallelism *int `yaml:"parallelism,omitempty"`
	// RetryBudget is how many times one attempt whose work provably never
	// began may be served before it is held for review. It breaks a loop
	// rather than tunes a rate, so its range is narrow: raising it far is
	// how a systematic failure gets paid for instead of found. Zero takes
	// the default.
	RetryBudget int `yaml:"retryBudget,omitempty"`
}

type Target struct {
	ID           string        `yaml:"id"`
	URL          string        `yaml:"url"`
	CredentialID string        `yaml:"credential"`
	Cache        TargetCache   `yaml:"cache"`
	RunnerGroup  string        `yaml:"runnerGroup"`
	Tiers        []TierBinding `yaml:"tiers"`
}

type TargetCache struct {
	Enabled    bool   `yaml:"enabled"`
	Generation string `yaml:"generation"`
}

// TierBinding creates or adopts one GitHub scale set. The scale-set name
// belongs to the target and tier pairing, never to the tier itself.
type TierBinding struct {
	TierID       string `yaml:"tier"`
	ScaleSetName string `yaml:"scaleSetName"`
}

// Credential is how a deployment proves it may administer runners on a
// target. A `token` credential is a person's: it carries their
// permissions, appears in their account and stops working when they
// leave. A `github_app` credential belongs to the organization instead.
//
// No secret value ever appears here. Every field is a reference to one.
type Credential struct {
	ID   string `yaml:"id"`
	Type string `yaml:"type"`
	// Exactly one of TokenEnv (an environment variable name) or TokenFile
	// (a mounted secret path) references the token of a `token`
	// credential; the value itself never appears in configuration.
	TokenEnv  string `yaml:"tokenEnv"`
	TokenFile string `yaml:"tokenFile"`
	// ClientID and InstallationID identify a `github_app` credential.
	// Neither is secret: the key is.
	ClientID       string `yaml:"clientID"`
	InstallationID int64  `yaml:"installationID"`
	// Exactly one of PrivateKeyEnv or PrivateKeyFile references the App's
	// private key, in PEM form. It is the longest-lived secret a
	// deployment holds and the one whose leak a single revocation cannot
	// contain, so a file is held to the same owner-only mode a token file
	// is.
	PrivateKeyEnv  string `yaml:"privateKeyEnv"`
	PrivateKeyFile string `yaml:"privateKeyFile"`
}

// Tier is a reusable local resource envelope, not a GitHub scale set.
type Tier struct {
	ID          string    `yaml:"id"`
	Parallelism int       `yaml:"parallelism"`
	Resources   Resources `yaml:"resources"`
	// JobTimeout bounds how long Runpool waits for one of this tier's
	// capsules. It is a backstop against a capsule that stops reporting,
	// not the job's own limit: the provider ends a job at its own timeout
	// and the runner exits, which resolves the lease long before this.
	// Empty takes DefaultJobTimeout.
	JobTimeout *Duration `yaml:"jobTimeout"`
	// CapsuleImage replaces the capsule this build ships for this tier's
	// jobs. Empty keeps the shipped one. It must be digest-qualified: the
	// controller launching an exact image it can name is the property the
	// shipped pin provides, and it is the one an operator's image has to
	// keep.
	CapsuleImage string `yaml:"capsuleImage"`
}

type Resources struct {
	CPU    CPUQuantity `yaml:"cpu"`
	Memory ByteSize    `yaml:"memory"`
	// Swap is additional swap above Memory. The Docker adapter translates
	// it to the daemon's total memory-plus-swap limit; zero disables swap.
	Swap ByteSize `yaml:"swap"`
	PIDs int64    `yaml:"pids"`
}

type Cache struct {
	Storage  CacheStorage  `yaml:"storage"`
	Global   CacheGlobal   `yaml:"global"`
	Defaults CacheDefaults `yaml:"defaults"`
}

type CacheStorage struct {
	Mode string `yaml:"mode"`
}

type CacheGlobal struct {
	MaxManagedBytes        ByteSize `yaml:"maxManagedBytes"`
	HighWatermarkPercent   int      `yaml:"highWatermarkPercent"`
	LowWatermarkPercent    int      `yaml:"lowWatermarkPercent"`
	SoftEmergencyFreeBytes ByteSize `yaml:"softEmergencyFreeBytes"`
	HardEmergencyFreeBytes ByteSize `yaml:"hardEmergencyFreeBytes"`
}

type CacheDefaults struct {
	RepositoryMaxBytes ByteSize `yaml:"repositoryMaxBytes"`
	UnusedTTL          Duration `yaml:"unusedTTL"`
}

type Observability struct {
	Log     LogConfig     `yaml:"log"`
	Metrics MetricsConfig `yaml:"metrics"`
}

type LogConfig struct {
	Format string `yaml:"format"`
	Level  string `yaml:"level"`
}

type MetricsConfig struct {
	Enabled bool `yaml:"enabled"`
}

type Network struct {
	Profile           string    `yaml:"profile"`
	IPv6              string    `yaml:"ipv6"`
	DNS               DNSConfig `yaml:"dns"`
	AllowPrivateCIDRs []CIDR    `yaml:"allowPrivateCIDRs"`
	DenyCIDRs         []CIDR    `yaml:"denyCIDRs"`
}

type DNSConfig struct {
	Mode string `yaml:"mode"`
}

// ByteSize is a byte quantity written as a non-negative integer with a
// binary (IEC) suffix: B, KiB, MiB, GiB or TiB. Decimal units and bare
// numbers are rejected so a config never means two different sizes.
type ByteSize int64

var byteUnits = []struct {
	suffix string
	factor int64
}{
	{"TiB", 1 << 40},
	{"GiB", 1 << 30},
	{"MiB", 1 << 20},
	{"KiB", 1 << 10},
	{"B", 1},
}

func ParseByteSize(s string) (ByteSize, error) {
	for _, u := range byteUnits {
		digits, ok := strings.CutSuffix(s, u.suffix)
		if !ok {
			continue
		}
		if !allDigits(digits) {
			break
		}
		n, err := strconv.ParseInt(digits, 10, 64)
		if err != nil || n > math.MaxInt64/u.factor {
			return 0, fmt.Errorf("byte quantity %q overflows", s)
		}
		return ByteSize(n * u.factor), nil
	}
	return 0, fmt.Errorf("invalid byte quantity %q: expected a non-negative integer with a B, KiB, MiB, GiB or TiB suffix", s)
}

func (b ByteSize) String() string {
	if b == 0 {
		return "0B"
	}
	for _, u := range byteUnits {
		if int64(b)%u.factor == 0 {
			return strconv.FormatInt(int64(b)/u.factor, 10) + u.suffix
		}
	}
	return strconv.FormatInt(int64(b), 10) + "B"
}

func (b *ByteSize) UnmarshalYAML(node *yaml.Node) error {
	v, err := ParseByteSize(node.Value)
	if err != nil {
		return err
	}
	*b = v
	return nil
}

func (b ByteSize) MarshalYAML() (any, error) { return b.String(), nil }

// CPUQuantity is a CPU count in nano-CPUs, written as a decimal such as
// "2" or "0.5" with at most nine fractional digits (Docker's NanoCPUs
// resolution). Parsing is exact; no floating point is involved.
type CPUQuantity int64

const nanoPerCPU = 1_000_000_000

func ParseCPUQuantity(s string) (CPUQuantity, error) {
	whole, frac, hasFrac := strings.Cut(s, ".")
	if !allDigits(whole) || len(whole) > 6 || (hasFrac && (!allDigits(frac) || len(frac) > 9)) {
		return 0, fmt.Errorf("invalid cpu quantity %q: expected a decimal count such as \"2\" or \"0.5\"", s)
	}
	n, _ := strconv.ParseInt(whole, 10, 64)
	nano := n * nanoPerCPU
	if hasFrac {
		f, _ := strconv.ParseInt(frac+strings.Repeat("0", 9-len(frac)), 10, 64)
		nano += f
	}
	return CPUQuantity(nano), nil
}

func (c CPUQuantity) String() string {
	whole, frac := int64(c)/nanoPerCPU, int64(c)%nanoPerCPU
	if frac == 0 {
		return strconv.FormatInt(whole, 10)
	}
	return fmt.Sprintf("%d.%s", whole, strings.TrimRight(fmt.Sprintf("%09d", frac), "0"))
}

func (c *CPUQuantity) UnmarshalYAML(node *yaml.Node) error {
	v, err := ParseCPUQuantity(node.Value)
	if err != nil {
		return err
	}
	*c = v
	return nil
}

func (c CPUQuantity) MarshalYAML() (any, error) { return c.String(), nil }

// Duration wraps time.Duration with strict Go duration syntax ("720h",
// "30m"); negative values are rejected at parse time.
type Duration time.Duration

func ParseDuration(s string) (Duration, error) {
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q: expected Go duration syntax such as \"720h\"", s)
	}
	if d < 0 {
		return 0, fmt.Errorf("invalid duration %q: must not be negative", s)
	}
	return Duration(d), nil
}

func (d Duration) String() string {
	s := time.Duration(d).String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	v, err := ParseDuration(node.Value)
	if err != nil {
		return err
	}
	*d = v
	return nil
}

func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

// CIDR is a network prefix; the address must be the network address, so a
// policy entry never silently means a wider or narrower range than written.
type CIDR struct {
	netip.Prefix
}

func ParseCIDR(s string) (CIDR, error) {
	p, err := netip.ParsePrefix(s)
	if err != nil {
		return CIDR{}, fmt.Errorf("invalid cidr %q", s)
	}
	if p != p.Masked() {
		return CIDR{}, fmt.Errorf("invalid cidr %q: address has host bits set; the network address is %s", s, p.Masked())
	}
	return CIDR{p}, nil
}

func (c *CIDR) UnmarshalYAML(node *yaml.Node) error {
	v, err := ParseCIDR(node.Value)
	if err != nil {
		return err
	}
	*c = v
	return nil
}

func (c CIDR) MarshalYAML() (any, error) { return c.Prefix.String(), nil }

type TargetScope string

const (
	ScopeRepository   TargetScope = "repository"
	ScopeOrganization TargetScope = "organization"
	// ScopeEnterprise is a scale set registered against an enterprise
	// rather than one of its organizations. The provider registers a
	// runner for it through a different endpoint from the other two.
	ScopeEnterprise TargetScope = "enterprise"
)

// TargetRef is a parsed target URL: the scope it names, the identifiers
// under that scope, and the canonical form rebuilt from the host that was
// given. Which hosts serve the protocol is not decided here; see
// ParseTargetURL.
type TargetRef struct {
	Scope TargetScope
	Owner string
	// Host is the lowercased host the URL named, carried so the surfaces
	// that must say where a credential travels — the startup log and the
	// doctor — do not re-parse the canonical URL to find out.
	Host         string
	Repository   string
	CanonicalURL string
}

// reservedTargetRoutes are the provider's own page prefixes. Kept short
// and certain: every entry 404s as an account on the REST API, so no
// legitimate owner is ever shadowed by the refusal.
var reservedTargetRoutes = map[string]bool{
	"apps":          true,
	"marketplace":   true,
	"notifications": true,
	"settings":      true,
	"sponsors":      true,
	"topics":        true,
}

var (
	slugRe    = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,62}[a-z0-9])?$`)
	envNameRe = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
	ownerRe   = regexp.MustCompile(`^[A-Za-z0-9-]+$`)
	repoRe    = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

func ParseTargetURL(raw string) (TargetRef, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return TargetRef{}, fmt.Errorf("invalid target url %q", raw)
	}
	switch {
	case u.Scheme != "https":
		return TargetRef{}, fmt.Errorf("invalid target url %q: scheme must be https", raw)
	case u.Host == "":
		return TargetRef{}, fmt.Errorf("invalid target url %q: no host", raw)
	case u.User != nil || u.RawQuery != "" || u.Fragment != "":
		return TargetRef{}, fmt.Errorf("invalid target url %q: credentials, query and fragment are not allowed", raw)
	}
	// The host is carried rather than checked. What serves the protocol is
	// a question the provider answers, and `runpool doctor` asks it with a
	// real call; a name refused here would refuse an Enterprise Server or
	// a data-residency host that speaks exactly the endpoints github.com
	// speaks. What is qualified is a separate statement, and the support
	// matrix is where it is made.
	host := strings.ToLower(u.Host)
	base := "https://" + host + "/"
	segments := strings.Split(strings.Trim(u.Path, "/"), "/")
	// Reserved routes: pages whose first segment can never be an account
	// — each verified unregistrable — so a pasted settings or
	// marketplace address is refused with its reason instead of being
	// read as an owner and failing much later against the provider.
	// "orgs" and "enterprises" are not here: they are addresses of the
	// scopes they name, and the cases below translate them.
	if len(segments) > 0 && reservedTargetRoutes[strings.ToLower(segments[0])] {
		return TargetRef{}, fmt.Errorf(
			"invalid target url %q: %q is a reserved route on the provider, not an owner", raw, segments[0])
	}
	switch {
	case len(segments) == 2 && strings.EqualFold(segments[0], "enterprises") && ownerRe.MatchString(segments[1]):
		return TargetRef{
			Scope:        ScopeEnterprise,
			Host:         host,
			Owner:        segments[1],
			CanonicalURL: base + "enterprises/" + segments[1],
		}, nil
	case len(segments) == 2 && strings.EqualFold(segments[0], "orgs") && ownerRe.MatchString(segments[1]):
		// The address a browser shows for an organization. Both segments
		// satisfy the owner and repository patterns, so without a case of
		// its own it is not refused — it is read as a repository named
		// after the organization and owned by "orgs". Nothing can be
		// named "orgs": the segment is a reserved path prefix, as
		// "enterprises" is, so no account is shadowed by reading it.
		return TargetRef{
			Scope:        ScopeOrganization,
			Host:         host,
			Owner:        segments[1],
			CanonicalURL: base + segments[1],
		}, nil
	case len(segments) == 1 && ownerRe.MatchString(segments[0]):
		return TargetRef{
			Scope:        ScopeOrganization,
			Host:         host,
			Owner:        segments[0],
			CanonicalURL: base + segments[0],
		}, nil
	case len(segments) == 2 && ownerRe.MatchString(segments[0]):
		// A clone URL carries a .git suffix that names no repository: the
		// API does not address one, so keeping it turns a recognisable
		// paste into a 404 against the provider long after the operator
		// has stopped looking at the URL they wrote.
		name := strings.TrimSuffix(segments[1], ".git")
		if !repoRe.MatchString(name) {
			break
		}
		return TargetRef{
			Scope:        ScopeRepository,
			Host:         host,
			Owner:        segments[0],
			Repository:   name,
			CanonicalURL: base + segments[0] + "/" + name,
		}, nil
	}
	return TargetRef{}, fmt.Errorf(
		"invalid target url %q: expected https://<host>/<owner>, https://<host>/<owner>/<repository> or https://<host>/enterprises/<name>",
		raw)
}

// digestQualifiedImage matches a reference pinned to a content digest,
// which is the only form that cannot move underneath the controller.
var digestQualifiedImage = regexp.MustCompile(`^.+@sha256:[a-f0-9]{64}$`)

// IsDigestQualifiedImage reports whether a reference names an exact
// image. It lives here because both the validator and the composition
// root decide the same thing about the same strings, and two copies of
// the rule is how one of them eventually accepts a tag.
func IsDigestQualifiedImage(ref string) bool {
	return digestQualifiedImage.MatchString(ref)
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// Image is what this tier's jobs run in: the tier's configured capsule
// where one is named, the shipped one everywhere else. The rule lives
// here, beside Ceiling, because two surfaces apply it — the binding that
// launches and the status document that reports — and two copies of a
// default rule are how a report drifts from what runs.
func (t Tier) Image(shipped string) string {
	if t.CapsuleImage != "" {
		return t.CapsuleImage
	}
	return shipped
}

// Ceiling is how long Runpool waits for one of this tier's capsules.
func (t Tier) Ceiling() time.Duration {
	if t.JobTimeout == nil {
		return time.Duration(DefaultJobTimeout)
	}
	return time.Duration(*t.JobTimeout)
}
