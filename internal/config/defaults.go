package config

// Enumerated values the schema accepts. V1 qualifies exactly one restricted
// network profile and one DNS mode; widening either is a design change.
const (
	HostTopologySharedDaemon    = "shared-daemon"
	HostTopologyDedicatedDaemon = "dedicated-daemon"

	// NetworkProfilePublicInternetOnly is the restricted profile: the
	// capsule has no route out, and its gateway relays what the policy
	// allows. It is implemented and tested live; release qualification on
	// the reference platform has not run.
	NetworkProfilePublicInternetOnly = "public-internet-only"
	// NetworkProfileUnsafeOpen builds no sandbox: the capsule reaches
	// whatever the host reaches. It is named so that no deployment can
	// claim containment it does not have.
	NetworkProfileUnsafeOpen = "unsafe-open-egress"
	IPv6Disabled             = "disabled"
	DNSModeGateway           = "gateway"
	CacheStorageModeVolume   = "volume"
	CredentialTypeToken      = "token"
	// CredentialTypeGitHubApp authenticates as an installation of a GitHub
	// App rather than as a person. The provider client mints and refreshes
	// the installation token itself from the App's own key.
	CredentialTypeGitHubApp = "github_app"

	LogFormatJSON = "json"
	LogFormatText = "text"

	DefaultInstanceName    = "primary"
	DefaultTierID          = "standard"
	DefaultCacheGeneration = "default"
	DefaultLogLevel        = "info"
	// MaxParallelism bounds allocator and preflight work for an untrusted
	// configuration while remaining far above a practical single-host limit.
	MaxParallelism = 10_000

	// DefaultJobTimeout is how long Runpool waits for a capsule that has
	// stopped reporting. A backstop below the provider's own 360-minute
	// maximum for a job ends work the provider still permits, and one
	// equal to it races the provider: the timeout that resolves a healthy
	// lease is the provider's, which then has to reach the runner, exit
	// it, and be observed here. Two hours of margin buys that sequence.
	DefaultJobTimeout = Duration(8 * 60 * 60 * 1e9) // 8h
	// MinJobTimeout and MaxJobTimeout bound the wait a tier may configure.
	//
	// The floor is low because preparation is bounded separately: a
	// ceiling governs the wait for a job the provider owns, and a tier
	// that knows its work is short gets its capacity back sooner. The cap
	// exists for the same reason the retry budget has one — a wedged
	// capsule holds a lane, an admission credit and a cache lane for the
	// whole ceiling, and a value with no upper bound is how that becomes
	// permanent by configuration.
	MinJobTimeout = Duration(60 * 1e9)           // 1m
	MaxJobTimeout = Duration(7 * 24 * 60 * 60e9) // 168h
	// MinRetryBudget and MaxRetryBudget bound what a deployment may set
	// for scheduling.retryBudget. The range is narrow because the budget
	// breaks a loop rather than tunes a rate: raising it far is how a
	// systematic failure gets paid for instead of found.
	MinRetryBudget = 1
	MaxRetryBudget = 10
	// DefaultLeaseHistory keeps a finished lease's record for 90 days.
	// Long enough to explain an incident from last quarter, short enough
	// that the books stay bounded by recent work on a host that runs for
	// years.
	DefaultLeaseHistory = Duration(90 * 24 * 60 * 60 * 1e9) // 2160h

	// MinLeaseHistory is the shortest configurable window. A retention of
	// minutes would delete the record of work an operator is still
	// looking at, which is a support problem rather than a saving.
	MinLeaseHistory = Duration(24 * 60 * 60 * 1e9) // 24h

	// DefaultScaleSetPrefix + tier id is the runs-on label workflows use
	// when a binding does not name its scale set explicitly.
	DefaultScaleSetPrefix = "runpool-"
)

var logLevels = []string{"debug", "info", "warn", "error"}

// ApplyDefaults fills unset optional fields in place. Mandatory fields
// (apiVersion, kind, targets, credentials, tiers) are never invented here;
// Validate reports them instead.
func ApplyDefaults(c *Config) {
	if c.Instance.Name == "" {
		c.Instance.Name = DefaultInstanceName
	}

	// On a dedicated daemon these are product defaults. A shared daemon
	// must state its reserve explicitly: Runpool cannot infer what the
	// colocated platform and production services need to remain healthy.
	if c.Host.Topology == HostTopologyDedicatedDaemon {
		r := &c.Host.Reserve
		if r.CPU == 0 {
			r.CPU = 1 * nanoPerCPU
		}
		if r.Memory == 0 {
			r.Memory = 2 << 30 // 2GiB
		}
		if r.FreeDisk == 0 {
			r.FreeDisk = 20 << 30 // 20GiB
		}
	}

	for i := range c.Targets {
		t := &c.Targets[i]
		if t.Cache.Enabled && t.Cache.Generation == "" {
			t.Cache.Generation = DefaultCacheGeneration
		}
		for j := range t.Tiers {
			b := &t.Tiers[j]
			if b.ScaleSetName == "" {
				b.ScaleSetName = DefaultScaleSetPrefix + b.TierID
			}
		}
	}

	for i := range c.Credentials {
		if c.Credentials[i].Type == "" {
			c.Credentials[i].Type = CredentialTypeToken
		}
	}

	for i := range c.Tiers {
		t := &c.Tiers[i]
		if t.Parallelism == 0 {
			t.Parallelism = 1
		}
		if t.Resources.CPU == 0 {
			t.Resources.CPU = 2 * nanoPerCPU
		}
		if t.Resources.Memory == 0 {
			t.Resources.Memory = 4 << 30 // 4GiB
		}
		if t.Resources.PIDs == 0 {
			t.Resources.PIDs = 1024
		}
	}

	if c.Cache.Storage.Mode == "" {
		c.Cache.Storage.Mode = CacheStorageModeVolume
	}
	g := &c.Cache.Global
	if g.MaxManagedBytes == 0 {
		g.MaxManagedBytes = 150 << 30 // 150GiB
	}
	if g.HighWatermarkPercent == 0 {
		g.HighWatermarkPercent = 80
	}
	if g.LowWatermarkPercent == 0 {
		g.LowWatermarkPercent = 65
	}
	if g.SoftEmergencyFreeBytes == 0 {
		g.SoftEmergencyFreeBytes = 20 << 30 // 20GiB
	}
	if g.HardEmergencyFreeBytes == 0 {
		g.HardEmergencyFreeBytes = 10 << 30 // 10GiB
	}
	d := &c.Cache.Defaults
	if d.RepositoryMaxBytes == 0 {
		d.RepositoryMaxBytes = 15 << 30 // 15GiB
	}
	if d.UnusedTTL == 0 {
		d.UnusedTTL = Duration(720 * 60 * 60 * 1e9) // 720h
	}

	// Materialised rather than left nil, because nothing downstream would
	// do it: an unset pointer would read as "keep forever", which is the
	// opposite of the default. Once set it always renders in `config
	// effective`, which is right — a deletion policy should not be
	// invisible.
	if c.Retention.LeaseHistory == nil {
		d := DefaultLeaseHistory
		c.Retention.LeaseHistory = &d
	}

	if c.Observability.Log.Format == "" {
		c.Observability.Log.Format = LogFormatJSON
	}
	if c.Observability.Log.Level == "" {
		c.Observability.Log.Level = DefaultLogLevel
	}

	n := &c.Network
	if n.Profile == "" {
		n.Profile = NetworkProfilePublicInternetOnly
	}
	if n.IPv6 == "" {
		n.IPv6 = IPv6Disabled
	}
	if n.DNS.Mode == "" {
		n.DNS.Mode = DNSModeGateway
	}
}

// The gateway's share of a lease's tier envelope, and the floor the
// capsule must be left with.
//
// A lease's budget covers everything a workload can cause to run. Under
// the restricted profile that includes its egress gateway: every
// connection the job opens is work the gateway performs. So the tier is
// split rather than duplicated — the gateway takes this fixed reserve,
// the capsule takes the rest, both sit under one parent cgroup, and the
// sum is the tier by construction.
//
// These live here, next to the validation that enforces them, because
// the rule is a property of a configured tier. The capsule package
// applies them.
const (
	GatewayReserveMemory = 128 << 20 // 128 MiB
	GatewayReserveCPUs   = 500_000_000
	GatewayReservePIDs   = 128
	// MinCapsuleMemory is what has to be left for a runner and a Docker
	// daemon after the gateway takes its share. Below this the capsule
	// cannot boot, so a tier that small is a configuration error rather
	// than a small tier.
	MinCapsuleMemory = 512 << 20
)
