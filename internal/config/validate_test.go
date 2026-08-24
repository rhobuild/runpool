package config

import (
	"os"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}

// validConfig returns a minimal configuration that passes validation after
// defaults; each test mutates one aspect and expects one class of failure.
func validConfig() *Config {
	c := &Config{
		APIVersion: APIVersion,
		Kind:       Kind,
		Host:       Host{Topology: HostTopologyDedicatedDaemon},
		Targets: []Target{{
			ID:           "app",
			URL:          "https://github.com/acme/app",
			CredentialID: "github-default",
			Cache:        TargetCache{Enabled: true},
			Tiers:        []TierBinding{{TierID: "standard"}},
		}},
		Credentials: []Credential{{ID: "github-default", TokenEnv: "RUNPOOL_GITHUB_TOKEN"}},
		Tiers:       []Tier{{ID: "standard"}},
	}
	ApplyDefaults(c)
	return c
}

func expectFieldError(t *testing.T, c *Config, wantPath string) {
	t.Helper()
	err := Validate(c)
	if err == nil {
		t.Fatalf("Validate succeeded; want error at %s", wantPath)
	}
	verr, ok := err.(*ValidationError)
	if !ok {
		t.Fatalf("error type %T; want *ValidationError", err)
	}
	for _, f := range verr.Fields {
		if strings.HasPrefix(f.Path, wantPath) {
			return
		}
	}
	t.Errorf("no error at %s; got: %v", wantPath, err)
}

func TestValidateBase(t *testing.T) {
	if err := Validate(validConfig()); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRules(t *testing.T) {
	cases := map[string]struct {
		mutate func(*Config)
		path   string
	}{
		"wrong apiVersion": {func(c *Config) { c.APIVersion = "v2" }, "apiVersion"},
		// The rejection rules below each survived deletion when this
		// table lacked their row -- verified by mutation, which is how a
		// validator's table has to be read: a rule with no row is a rule
		// review believes in and nothing holds.
		"wrong kind": {func(c *Config) { c.Kind = "Deployment" }, "kind"},
		"instance name is not a slug": {func(c *Config) {
			c.Instance.Name = "Prod Instance"
		}, "instance.name"},
		"duplicate tier id": {func(c *Config) {
			c.Tiers = append(c.Tiers, c.Tiers[0])
		}, "tiers[1].id"},
		"no tiers at all":       {func(c *Config) { c.Tiers = nil }, "tiers"},
		"no targets at all":     {func(c *Config) { c.Targets = nil }, "targets"},
		"no credentials at all": {func(c *Config) { c.Credentials = nil }, "credentials"},
		"duplicate credential id": {func(c *Config) {
			// Resolution by id is last-wins where credentials are read,
			// so two entries under one id would have a target silently
			// authenticate with whichever came later.
			c.Credentials = append(c.Credentials, c.Credentials[0])
		}, "credentials[1].id"},
		"duplicate target id": {func(c *Config) {
			dup := c.Targets[0]
			dup.URL = "https://github.com/acme/other"
			c.Targets = append(c.Targets, dup)
		}, "targets[1].id"},
		"scale set name is not a slug": {func(c *Config) {
			c.Targets[0].Tiers[0].ScaleSetName = "Runpool Standard"
		}, "targets[0].tiers[0].scaleSetName"},
		"no tier bindings": {func(c *Config) { c.Targets[0].Tiers = nil }, "targets[0].tiers"},
		"negative host reserve": {func(c *Config) {
			c.Host.Reserve.CPU = CPUQuantity(-1)
		}, "host.reserve"},
		"zero cpu":    {func(c *Config) { c.Tiers[0].Resources.CPU = 0 }, "tiers[0].resources.cpu"},
		"zero memory": {func(c *Config) { c.Tiers[0].Resources.Memory = 0 }, "tiers[0].resources.memory"},
		"zero pids":   {func(c *Config) { c.Tiers[0].Resources.PIDs = 0 }, "tiers[0].resources.pids"},
		"metrics cannot be enabled": {func(c *Config) {
			c.Observability.Metrics.Enabled = true
		}, "observability.metrics.enabled"},
		// The envelope floors. Under the restricted profile a tier holds
		// the capsule and its gateway together, and memory had a usable-
		// remainder floor where cpu and pids required only more-than-the-
		// reserve: a tier of cpu "0.500000001" validated and handed the
		// capsule one nano-CPU.
		"memory below the gateway reserve plus the capsule floor": {func(c *Config) {
			c.Tiers[0].Resources.Memory = ByteSize(GatewayReserveMemory + MinCapsuleMemory - 1)
		}, "tiers[0].resources.memory"},
		"cpu leaves the capsule an unusable share": {func(c *Config) {
			c.Tiers[0].Resources.CPU = CPUQuantity(GatewayReserveCPUs + 1)
		}, "tiers[0].resources.cpu"},
		"pids leave the capsule an unusable share": {func(c *Config) {
			c.Tiers[0].Resources.PIDs = GatewayReservePIDs + 1
		}, "tiers[0].resources.pids"},
		"cache on an enterprise target": {func(c *Config) {
			c.Targets[0].URL = "https://github.com/enterprises/acme"
			c.Targets[0].RunnerGroup = "runpool"
			c.Targets[0].Cache.Enabled = true
		}, "targets[0].cache.enabled"},
		"runner group on a repository target": {func(c *Config) {
			c.Targets[0].RunnerGroup = "runpool"
		}, "targets[0].runnerGroup"},
		"retry budget above the range": {func(c *Config) {
			c.Scheduling.RetryBudget = MaxRetryBudget + 1
		}, "scheduling.retryBudget"},
		"retry budget below the range": {func(c *Config) {
			c.Scheduling.RetryBudget = -1
		}, "scheduling.retryBudget"},
		"job ceiling above the cap": {func(c *Config) {
			d := MaxJobTimeout + Duration(time.Hour)
			c.Tiers[0].JobTimeout = &d
		}, "tiers[0].jobTimeout"},
		"job ceiling below the floor": {func(c *Config) {
			d := Duration(time.Second)
			c.Tiers[0].JobTimeout = &d
		}, "tiers[0].jobTimeout"},
		"unknown credential type": {func(c *Config) {
			c.Credentials[0].Type = "oauth"
		}, "credentials[0].type"},
		"app credential without a client id": {func(c *Config) {
			c.Credentials[0] = Credential{ID: "app", Type: CredentialTypeGitHubApp,
				InstallationID: 1, PrivateKeyEnv: "KEY"}
		}, "credentials[0].clientID"},
		"app credential without an installation": {func(c *Config) {
			c.Credentials[0] = Credential{ID: "app", Type: CredentialTypeGitHubApp,
				ClientID: "Iv1.a", PrivateKeyEnv: "KEY"}
		}, "credentials[0].installationID"},
		"app credential with no key reference": {func(c *Config) {
			c.Credentials[0] = Credential{ID: "app", Type: CredentialTypeGitHubApp,
				ClientID: "Iv1.a", InstallationID: 1}
		}, "credentials[0]"},
		"app credential with both key references": {func(c *Config) {
			c.Credentials[0] = Credential{ID: "app", Type: CredentialTypeGitHubApp,
				ClientID: "Iv1.a", InstallationID: 1,
				PrivateKeyEnv: "KEY", PrivateKeyFile: "/run/secrets/app.pem"}
		}, "credentials[0]"},
		"app credential with a relative key path": {func(c *Config) {
			c.Credentials[0] = Credential{ID: "app", Type: CredentialTypeGitHubApp,
				ClientID: "Iv1.a", InstallationID: 1, PrivateKeyFile: "secrets/app.pem"}
		}, "credentials[0].privateKeyFile"},
		"app credential also carrying a token": {func(c *Config) {
			c.Credentials[0] = Credential{ID: "app", Type: CredentialTypeGitHubApp,
				ClientID: "Iv1.a", InstallationID: 1, PrivateKeyEnv: "KEY",
				TokenEnv: "TOKEN"}
		}, "credentials[0]"},
		"token credential carrying app fields": {func(c *Config) {
			c.Credentials[0].ClientID = "Iv1.a"
		}, "credentials[0]"},
		"capsule image by tag": {func(c *Config) {
			c.Tiers[0].CapsuleImage = "ghcr.io/acme/capsule:v1"
		}, "tiers[0].capsuleImage"},
		"capsule image by name alone": {func(c *Config) {
			c.Tiers[0].CapsuleImage = "acme-capsule"
		}, "tiers[0].capsuleImage"},
		"capsule image with a short digest": {func(c *Config) {
			c.Tiers[0].CapsuleImage = "ghcr.io/acme/capsule@sha256:abc123"
		}, "tiers[0].capsuleImage"},
		"missing host topology": {func(c *Config) { c.Host.Topology = "" }, "host.topology"},
		"shared daemon without reserve": {func(c *Config) {
			c.Host.Topology = HostTopologySharedDaemon
			c.Host.Reserve = Reserve{}
		}, "host.reserve"},
		"shared daemon with unsafe egress": {func(c *Config) {
			c.Host.Topology = HostTopologySharedDaemon
			c.Network.Profile = NetworkProfileUnsafeOpen
		}, "network.profile"},
		"shared organization without runner group": {func(c *Config) {
			c.Host.Topology = HostTopologySharedDaemon
			c.Targets[0].URL = "https://github.com/acme"
			c.Targets[0].Cache.Enabled = false
		}, "targets[0].runnerGroup"},
		"shared enterprise without runner group": {func(c *Config) {
			c.Host.Topology = HostTopologySharedDaemon
			c.Targets[0].URL = "https://github.com/enterprises/acme"
			c.Targets[0].Cache.Enabled = false
		}, "targets[0].runnerGroup"},
		"org with cache": {func(c *Config) {
			c.Targets[0].URL = "https://github.com/acme"
		}, "targets[0].cache.enabled"},
		"runner group on repository": {func(c *Config) {
			c.Targets[0].RunnerGroup = "default"
		}, "targets[0].runnerGroup"},
		"unknown tier": {func(c *Config) {
			c.Targets[0].Tiers[0].TierID = "huge"
		}, "targets[0].tiers[0].tier"},
		"unknown credential": {func(c *Config) {
			c.Targets[0].CredentialID = "nope"
		}, "targets[0].credential"},
		"duplicate target url": {func(c *Config) {
			dup := c.Targets[0]
			dup.ID = "again"
			c.Targets = append(c.Targets, dup)
		}, "targets[1].url"},
		// GitHub logins and repository names are case-insensitive, so two
		// targets differing only in case are one scope. Left as duplicates
		// they each get a binding key of their own, and the scale-set name
		// check below is written on the premise that target URLs are
		// unique — so both bindings would carry the same default name and
		// collide on one remote scale set.
		"the same target under another case": {func(c *Config) {
			dup := c.Targets[0]
			dup.ID = "again"
			dup.URL = "https://github.com/Acme/App"
			c.Targets = append(c.Targets, dup)
		}, "targets[1].url"},
		"duplicate scale-set name in scope": {func(c *Config) {
			c.Tiers = append(c.Tiers, Tier{ID: "heavy"})
			ApplyDefaults(c)
			c.Targets[0].Tiers = append(c.Targets[0].Tiers, TierBinding{TierID: "heavy", ScaleSetName: "runpool-standard"})
		}, "targets[0].tiers[1].scaleSetName"},
		"credential with both sources": {func(c *Config) {
			c.Credentials[0].TokenFile = "/run/secrets/token"
		}, "credentials[0]"},
		"credential with no source": {func(c *Config) {
			c.Credentials[0].TokenEnv = ""
		}, "credentials[0]"},
		"unused tier": {func(c *Config) {
			c.Tiers = append(c.Tiers, Tier{ID: "idle"})
			ApplyDefaults(c)
		}, "tiers"},
		"unused credential": {func(c *Config) {
			c.Credentials = append(c.Credentials, Credential{ID: "spare", TokenEnv: "OTHER_TOKEN"})
		}, "credentials"},
		"negative swap": {func(c *Config) {
			c.Tiers[0].Resources.Swap = -1
		}, "tiers[0].resources.swap"},
		"negative tier parallelism": {func(c *Config) { c.Tiers[0].Parallelism = -1 }, "tiers[0].parallelism"},
		"tier parallelism above input bound": {func(c *Config) {
			c.Tiers[0].Parallelism = MaxParallelism + 1
		}, "tiers[0].parallelism"},
		"zero global parallelism": {func(c *Config) {
			zero := 0
			c.Scheduling.Parallelism = &zero
		}, "scheduling.parallelism"},
		"global parallelism above tier total": {func(c *Config) {
			two := 2
			c.Scheduling.Parallelism = &two
		}, "scheduling.parallelism"},
		"inverted watermarks": {func(c *Config) {
			c.Cache.Global.LowWatermarkPercent = 90
		}, "cache.global"},
		"hard emergency above soft": {func(c *Config) {
			c.Cache.Global.HardEmergencyFreeBytes = c.Cache.Global.SoftEmergencyFreeBytes * 2
		}, "cache.global.hardEmergencyFreeBytes"},
		"unqualified network profile": {func(c *Config) {
			c.Network.Profile = "standard"
		}, "network.profile"},
		"invalid ipv6 mode": {func(c *Config) { c.Network.IPv6 = "on" }, "network.ipv6"},
		"bind storage not yet supported": {func(c *Config) {
			c.Cache.Storage.Mode = "bind"
		}, "cache.storage.mode"},
		"bad log level": {func(c *Config) {
			c.Observability.Log.Level = "trace"
		}, "observability.log.level"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := validConfig()
			tc.mutate(c)
			expectFieldError(t, c, tc.path)
		})
	}
}

// A digest-qualified capsule image is accepted: the field exists so an
// operator can add tools to their jobs, and refusing every value would be
// the same as not having it.
func TestADigestQualifiedCapsuleImageIsAccepted(t *testing.T) {
	c := validConfig()
	c.Tiers[0].CapsuleImage = "ghcr.io/acme/capsule@sha256:" +
		"3333333333333333333333333333333333333333333333333333333333333333"
	if err := Validate(c); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// A GitHub App credential is accepted whole: an organization running on
// one is the case the type exists for, and refusing every shape would be
// the same as not having it.
func TestAGitHubAppCredentialIsAccepted(t *testing.T) {
	c := validConfig()
	c.Credentials[0] = Credential{
		ID: "runners", Type: CredentialTypeGitHubApp,
		ClientID: "Iv1.0123456789abcdef", InstallationID: 12345678,
		PrivateKeyFile: "/run/secrets/runpool/app.pem",
	}
	c.Targets[0].CredentialID = "runners"
	if err := Validate(c); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// An enterprise target is accepted whole. The runner-group rule it
// carries on a shared daemon is in the table above, beside the
// organization case it matches.
func TestAnEnterpriseTargetIsAccepted(t *testing.T) {
	c := validConfig()
	c.Targets[0].URL = "https://github.com/enterprises/acme"
	c.Targets[0].RunnerGroup = "runpool"
	c.Targets[0].Cache.Enabled = false
	if err := Validate(c); err != nil {
		t.Fatalf("Validate: %v", err)
	}

}

func TestValidateAggregatesErrors(t *testing.T) {
	c := validConfig()
	c.APIVersion = "nope"
	c.Network.IPv6 = "on"
	err := Validate(c)
	verr, ok := err.(*ValidationError)
	if !ok || len(verr.Fields) < 2 {
		t.Fatalf("expected aggregated errors, got: %v", err)
	}
}

// TestNetworkCIDRValidation: allowPrivateCIDRs punches holes through the
// baseline deny, and allow is consulted before deny at decision time. A
// prefix wider than what the baseline withholds therefore reopens egress
// the restricted profile exists to close — silently, with `network.profile`
// still reading public-internet-only. IPv6 is rejected because the ruleset
// it renders into is IPv4 only; accepting it moves the failure to gateway
// boot, pointing at iptables instead of at the offending line.
func TestNetworkCIDRValidation(t *testing.T) {
	for name, tc := range map[string]struct {
		allow, deny string
		wantErr     bool
	}{
		"private range is the intended use": {allow: "10.0.0.0/8"},
		"narrower than the baseline":        {allow: "192.168.5.0/24"},
		"link-local may be reopened":        {allow: "169.254.169.254/32"},
		// Public space is a no-op: the profile already permits the public
		// internet. This is how an operator reaches a service on the host's
		// own public address, which BuildDeny withholds at runtime from
		// facts static validation cannot see.
		"public space is a no-op":     {allow: "8.8.8.0/24"},
		"host's own public address":   {allow: "203.0.113.7/32"},
		"everything":                  {allow: "0.0.0.0/0", wantErr: true},
		"swallows a whole deny range": {allow: "10.0.0.0/7", wantErr: true},
		"swallows link-local":         {allow: "169.254.0.0/15", wantErr: true},
		// Exactly the withheld range is not broader than anything, so the
		// rule above lets it through: link-local carries its own, and it
		// has to be checked here as well as on the policy, because this
		// is the file an operator edits.
		"the whole of link-local": {allow: "169.254.0.0/16", wantErr: true},
		"half of link-local":      {allow: "169.254.0.0/17", wantErr: true},
		"ipv6 allow":              {allow: "fd00::/8", wantErr: true},
		// The v4-in-v6 form unmaps to IPv4 and then matches no address at
		// decision time, while rendering into the ruleset verbatim.
		"mapped v4 allow":   {allow: "::ffff:198.18.5.0/120", wantErr: true},
		"mapped v4 deny":    {deny: "::ffff:198.18.5.0/120", wantErr: true},
		"ipv4 deny is fine": {deny: "203.0.113.0/24"},
		"ipv6 deny":         {deny: "fd00::/8", wantErr: true},
	} {
		cfg := validConfig()
		if tc.allow != "" {
			p, err := ParseCIDR(tc.allow)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			cfg.Network.AllowPrivateCIDRs = []CIDR{p}
		}
		if tc.deny != "" {
			p, err := ParseCIDR(tc.deny)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			cfg.Network.DenyCIDRs = []CIDR{p}
		}
		err := Validate(cfg)
		if tc.wantErr && err == nil {
			t.Errorf("%s: accepted; want a validation error", name)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("%s: rejected: %v", name, err)
		}
	}
}

// TestLeaseHistoryRetention: zero is a decision here, not an absence, so
// the `> 0` rule the cache TTL uses does not apply. The pointer is what
// separates "keep every record" from "not configured" — without it,
// defaulting would silently overwrite the operator's choice.
func TestLeaseHistoryRetention(t *testing.T) {
	// Omitted takes the default, materialised rather than left nil.
	c := &Config{}
	ApplyDefaults(c)
	if c.Retention.LeaseHistory == nil {
		t.Fatal("an omitted retention window was left nil; nothing downstream would default it")
	}
	if *c.Retention.LeaseHistory != DefaultLeaseHistory {
		t.Errorf("default = %s; want %s", c.Retention.LeaseHistory, DefaultLeaseHistory)
	}

	// Zero survives defaulting and validation: it means keep forever.
	keep := Duration(0)
	c = validConfig()
	c.Retention.LeaseHistory = &keep
	ApplyDefaults(c)
	if c.Retention.LeaseHistory == nil || *c.Retention.LeaseHistory != 0 {
		t.Fatalf("zero was overwritten by defaulting: %v", c.Retention.LeaseHistory)
	}
	if err := Validate(c); err != nil {
		t.Errorf("zero retention rejected: %v", err)
	}

	for name, d := range map[string]Duration{
		"negative":      Duration(-time.Hour),
		"below the min": MinLeaseHistory - 1,
	} {
		c := validConfig()
		v := d
		c.Retention.LeaseHistory = &v
		err := Validate(c)
		if err == nil {
			t.Errorf("%s retention was accepted", name)
			continue
		}
		if !strings.Contains(err.Error(), "retention.leaseHistory") {
			t.Errorf("%s: error %q does not name the field", name, err)
		}
	}
}

// The effective document has to be loadable: a value that does not
// survive marshal-then-parse means `config effective` prints something
// the loader would reject.
func TestLeaseHistoryRoundTripsThroughTheEffectiveDocument(t *testing.T) {
	for _, d := range []Duration{DefaultLeaseHistory, 0, MinLeaseHistory} {
		v := d
		in := Retention{LeaseHistory: &v}
		out, err := yaml.Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		var back Retention
		if err := yaml.Unmarshal(out, &back); err != nil {
			t.Fatalf("re-reading %q: %v", out, err)
		}
		if back.LeaseHistory == nil || *back.LeaseHistory != d {
			t.Errorf("round trip changed %s: rendered %q, read back %v", d, out, back.LeaseHistory)
		}
	}
}

// TestJobCeiling: the ceiling is Runpool's wait for a capsule that
// stopped reporting, not the job's own limit, so its default clears the
// provider's maximum for a job — a backstop below that ends work the
// provider still permits.
func TestJobCeiling(t *testing.T) {
	const providerJobMaximum = 360 * time.Minute
	if d := time.Duration(DefaultJobTimeout); d <= providerJobMaximum {
		t.Errorf("default ceiling is %s, which is not above the provider's own %s maximum",
			d, providerJobMaximum)
	}
	if got := (Tier{}).Ceiling(); got != time.Duration(DefaultJobTimeout) {
		t.Errorf("a tier naming no ceiling waits %s, want the default", got)
	}
	own := Duration(90 * time.Minute)
	if got := (Tier{JobTimeout: &own}).Ceiling(); got != 90*time.Minute {
		t.Errorf("a tier naming a ceiling waits %s, want its own", got)
	}
}

// TestTheEnvelopeFloorsApplyOnlyWhereAGatewayExists: under
// unsafe-open-egress no gateway is built and the whole tier is the
// capsule's, so the floors above the plain positivity checks would
// refuse tiers the profile can serve.
func TestTheEnvelopeFloorsApplyOnlyWhereAGatewayExists(t *testing.T) {
	c := validConfig()
	c.Network.Profile = NetworkProfileUnsafeOpen
	c.Tiers[0].Resources.Memory = ByteSize(GatewayReserveMemory + MinCapsuleMemory - 1)
	c.Tiers[0].Resources.CPU = CPUQuantity(GatewayReserveCPUs + 1)
	c.Tiers[0].Resources.PIDs = GatewayReservePIDs + 1
	if err := Validate(c); err != nil {
		t.Errorf("a small tier under unsafe-open-egress was refused: %v", err)
	}
}
