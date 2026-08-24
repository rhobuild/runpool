package config

import (
	"fmt"
	"strconv"
)

// Quick Start environment variables. They compile into the same schema the
// advanced YAML file uses; there is no second configuration model.
const (
	EnvGitHubURL           = "RUNPOOL_GITHUB_URL"
	EnvGitHubToken         = "RUNPOOL_GITHUB_TOKEN"
	EnvGitHubTokenFile     = "RUNPOOL_GITHUB_TOKEN_FILE"
	EnvGitHubRunnerGroup   = "RUNPOOL_GITHUB_RUNNER_GROUP"
	EnvHostTopology        = "RUNPOOL_HOST_TOPOLOGY"
	EnvHostReserveCPU      = "RUNPOOL_HOST_RESERVE_CPU"
	EnvHostReserveMemory   = "RUNPOOL_HOST_RESERVE_MEMORY"
	EnvHostReserveSwap     = "RUNPOOL_HOST_RESERVE_SWAP"
	EnvHostReserveFreeDisk = "RUNPOOL_HOST_RESERVE_FREE_DISK"
	EnvTier                = "RUNPOOL_TIER"
	EnvParallelism         = "RUNPOOL_PARALLELISM"
	EnvLogLevel            = "RUNPOOL_LOG_LEVEL"
	EnvNetworkProfile      = "RUNPOOL_NETWORK_PROFILE"
	EnvConfigFile          = "RUNPOOL_CONFIG_FILE"
)

// quickStartTargetVars conflict with file mode: when RUNPOOL_CONFIG_FILE is
// set, every setting a Quick Start variable carries comes from the file
// alone.
//
// The list is every one of them but RUNPOOL_GITHUB_TOKEN, including
// those the file also has a place for — logging and the network
// profile. A variable that is neither applied nor refused is the worst
// of the three outcomes: an operator who set RUNPOOL_LOG_LEVEL=debug
// beside a configuration file gets no debug logging and no explanation,
// and looks for the reason somewhere else. The token is the one
// exemption, because a file's tokenEnv may legitimately name it; the
// token file path had no such excuse and was silently ignored anyway.
var quickStartTargetVars = []string{
	EnvGitHubURL, EnvGitHubRunnerGroup, EnvGitHubTokenFile, EnvHostTopology, EnvHostReserveCPU,
	EnvHostReserveMemory, EnvHostReserveSwap, EnvHostReserveFreeDisk, EnvTier, EnvParallelism,
	EnvLogLevel, EnvNetworkProfile,
}

// FromEnvironment translates the Quick Start variables into a Config.
// The token value is never read here — the credential records only the
// environment variable name or file path that references it.
func FromEnvironment(environ func(string) string) (*Config, error) {
	rawURL := environ(EnvGitHubURL)
	if rawURL == "" {
		return nil, fmt.Errorf("%s is required: set it to https://github.com/<owner> or https://github.com/<owner>/<repository>", EnvGitHubURL)
	}
	ref, err := ParseTargetURL(rawURL)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", EnvGitHubURL, err)
	}

	token, tokenFile := environ(EnvGitHubToken), environ(EnvGitHubTokenFile)
	switch {
	case token == "" && tokenFile == "":
		return nil, fmt.Errorf("either %s or %s is required", EnvGitHubToken, EnvGitHubTokenFile)
	case token != "" && tokenFile != "":
		return nil, fmt.Errorf("%s and %s are mutually exclusive", EnvGitHubToken, EnvGitHubTokenFile)
	}
	topology := environ(EnvHostTopology)
	if topology == "" {
		return nil, fmt.Errorf("%s is required: set %q when sharing a platform daemon or %q on an exclusive CI host",
			EnvHostTopology, HostTopologySharedDaemon, HostTopologyDedicatedDaemon)
	}
	credential := Credential{ID: "github-default", Type: CredentialTypeToken}
	if token != "" {
		credential.TokenEnv = EnvGitHubToken
	} else {
		credential.TokenFile = tokenFile
	}

	tier := environ(EnvTier)
	if tier == "" {
		tier = DefaultTierID
	}

	parallelism := 1
	if raw := environ(EnvParallelism); raw != "" {
		parallelism, err = strconv.Atoi(raw)
		if err != nil || parallelism < 1 {
			return nil, fmt.Errorf("%s: expected an integer >= 1, got %q", EnvParallelism, raw)
		}
	}

	c := &Config{
		APIVersion: APIVersion,
		Kind:       Kind,
		Host:       Host{Topology: HostTopology(topology)},
		Targets: []Target{{
			ID:           "default",
			URL:          ref.CanonicalURL,
			CredentialID: credential.ID,
			// Cache stays off in Quick Start until reuse across jobs is
			// release-qualified on the reference deployment: a default must not
			// promise what the runtime has not demonstrated. A repository
			// target can still enable it explicitly in the advanced
			// configuration; an organization target never can, because
			// its runners are not bound to the repository whose cache
			// they would mount.
			Cache:       TargetCache{Enabled: false},
			RunnerGroup: environ(EnvGitHubRunnerGroup),
			Tiers:       []TierBinding{{TierID: tier}},
		}},
		Credentials: []Credential{credential},
		Tiers:       []Tier{{ID: tier, Parallelism: parallelism}},
	}
	if raw := environ(EnvHostReserveCPU); raw != "" {
		c.Host.Reserve.CPU, err = ParseCPUQuantity(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", EnvHostReserveCPU, err)
		}
	}
	if raw := environ(EnvHostReserveMemory); raw != "" {
		c.Host.Reserve.Memory, err = ParseByteSize(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", EnvHostReserveMemory, err)
		}
	}
	if raw := environ(EnvHostReserveSwap); raw != "" {
		c.Host.Reserve.Swap, err = ParseByteSize(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", EnvHostReserveSwap, err)
		}
	}
	if raw := environ(EnvHostReserveFreeDisk); raw != "" {
		c.Host.Reserve.FreeDisk, err = ParseByteSize(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", EnvHostReserveFreeDisk, err)
		}
	}
	if level := environ(EnvLogLevel); level != "" {
		c.Observability.Log.Level = LogLevel(level)
	}
	if profile := environ(EnvNetworkProfile); profile != "" {
		c.Network.Profile = NetworkProfile(profile)
	}
	return c, nil
}
