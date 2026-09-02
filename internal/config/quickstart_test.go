package config

import (
	"slices"
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string {
	return func(name string) string { return m[name] }
}

func TestQuickStartMinimal(t *testing.T) {
	c, err := Load(env(map[string]string{
		EnvGitHubURL:    "https://github.com/acme/app",
		EnvGitHubToken:  "secret",
		EnvHostTopology: string(HostTopologyDedicatedDaemon),
	}))
	if err != nil {
		t.Fatal(err)
	}
	target := c.Targets[0]
	// Quick Start leaves the cache off: a lane is durable state shared by
	// every later job for its repository, which an operator opts into.
	if target.Cache.Enabled {
		t.Error("Quick Start enabled a cache lane the operator did not ask for")
	}
	if got := target.Tiers[0].ScaleSetName; got != "runpool-standard" {
		t.Errorf("scaleSetName = %q; want runpool-standard", got)
	}
	if c.Tiers[0].Parallelism != 1 {
		t.Errorf("parallelism = %d; want 1", c.Tiers[0].Parallelism)
	}
	if cr := c.Credentials[0]; cr.TokenEnv != EnvGitHubToken || cr.TokenFile != "" {
		t.Errorf("credential should reference the env var by name, got %+v", cr)
	}
	if c.Network.Profile != NetworkProfilePublicInternetOnly {
		t.Errorf("network profile = %q", c.Network.Profile)
	}
}

func TestQuickStartOrganizationDisablesCache(t *testing.T) {
	c, err := Load(env(map[string]string{
		EnvGitHubURL:         "https://github.com/acme",
		EnvGitHubToken:       "secret",
		EnvGitHubRunnerGroup: "runpool",
		EnvHostTopology:      string(HostTopologyDedicatedDaemon),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Targets[0].Cache.Enabled {
		t.Error("organization target must not enable a repository cache")
	}
}

func TestQuickStartErrors(t *testing.T) {
	cases := map[string]map[string]string{
		"missing topology": {EnvGitHubURL: "https://github.com/acme/app", EnvGitHubToken: "secret"},
		"missing url":      {EnvGitHubToken: "secret", EnvHostTopology: string(HostTopologyDedicatedDaemon)},
		"missing token":    {EnvGitHubURL: "https://github.com/acme/app", EnvHostTopology: string(HostTopologyDedicatedDaemon)},
		"token and file": {
			EnvGitHubURL:       "https://github.com/acme/app",
			EnvGitHubToken:     "secret",
			EnvGitHubTokenFile: "/run/secrets/token",
			EnvHostTopology:    string(HostTopologyDedicatedDaemon),
		},
		"bad parallelism": {
			EnvGitHubURL:    "https://github.com/acme/app",
			EnvGitHubToken:  "secret",
			EnvParallelism:  "zero",
			EnvHostTopology: string(HostTopologyDedicatedDaemon),
		},
		"config file with target var": {
			EnvConfigFile: "testdata/example.yaml",
			EnvGitHubURL:  "https://github.com/acme/app",
		},
	}
	for name, vars := range cases {
		if _, err := Load(env(vars)); err == nil {
			t.Errorf("%s: Load succeeded; want error", name)
		}
	}
}

func TestQuickStartTokenFile(t *testing.T) {
	c, err := Load(env(map[string]string{
		EnvGitHubURL:       "https://github.com/acme/app",
		EnvGitHubTokenFile: "/run/secrets/github-token",
		EnvHostTopology:    string(HostTopologyDedicatedDaemon),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cr := c.Credentials[0]; cr.TokenFile != "/run/secrets/github-token" || cr.TokenEnv != "" {
		t.Errorf("credential = %+v", cr)
	}
}

func TestQuickStartTokenFilePermissionPolicy(t *testing.T) {
	c, err := Load(env(map[string]string{
		EnvGitHubURL:                 "https://github.com/acme/app",
		EnvGitHubTokenFile:           "/run/secrets/github-token",
		EnvCredentialFilePermissions: string(CredentialFilePermissionsAllowWorldRead),
		EnvHostTopology:              string(HostTopologyDedicatedDaemon),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Credentials[0].FilePermissions; got != CredentialFilePermissionsAllowWorldRead {
		t.Errorf("filePermissions = %q", got)
	}

	_, err = Load(env(map[string]string{
		EnvGitHubURL:                 "https://github.com/acme/app",
		EnvGitHubToken:               "secret",
		EnvCredentialFilePermissions: string(CredentialFilePermissionsAllowWorldRead),
		EnvHostTopology:              string(HostTopologyDedicatedDaemon),
	}))
	if err == nil || !strings.Contains(err.Error(), "filePermissions") {
		t.Fatalf("environment credential with a file-mode policy: %v", err)
	}

	_, err = Load(env(map[string]string{
		EnvGitHubURL:                 "https://github.com/acme/app",
		EnvGitHubToken:               "secret",
		EnvCredentialFilePermissions: string(CredentialFilePermissionsOwnerOnly),
		EnvHostTopology:              string(HostTopologyDedicatedDaemon),
	}))
	if err == nil || !strings.Contains(err.Error(), "filePermissions") {
		t.Fatalf("owner-only on an environment credential: %v", err)
	}
}

func TestQuickStartSharedDaemonRequiresReserve(t *testing.T) {
	base := map[string]string{
		EnvGitHubURL:    "https://github.com/acme/app",
		EnvGitHubToken:  "secret",
		EnvHostTopology: string(HostTopologySharedDaemon),
	}
	if _, err := Load(env(base)); err == nil || !strings.Contains(err.Error(), "host.reserve") {
		t.Fatalf("shared daemon without reserve: %v", err)
	}
	base[EnvHostReserveCPU] = "1"
	base[EnvHostReserveMemory] = "2GiB"
	base[EnvHostReserveFreeDisk] = "20GiB"
	c, err := Load(env(base))
	if err != nil {
		t.Fatal(err)
	}
	if c.Host.Topology != HostTopologySharedDaemon {
		t.Fatalf("topology = %q", c.Host.Topology)
	}
}

func TestLoadFileMode(t *testing.T) {
	c, err := Load(env(map[string]string{EnvConfigFile: "testdata/example.yaml"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Targets) != 2 {
		t.Fatalf("targets = %d; want 2", len(c.Targets))
	}
	// The same human-friendly scale-set name in different GitHub scopes
	// names different resources and must be accepted.
	if c.Targets[0].Tiers[0].ScaleSetName != c.Targets[1].Tiers[0].ScaleSetName {
		t.Error("example should exercise cross-scope name reuse")
	}
}

func TestLoadFileUnknownField(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/bad.yaml"
	content := "apiVersion: runpool.rhobuild.com/v1\nkind: RunpoolConfig\nfoo: 1\n"
	if err := writeFile(path, content); err != nil {
		t.Fatal(err)
	}
	_, err := LoadFile(path)
	if err == nil || !strings.Contains(err.Error(), "foo") {
		t.Errorf("unknown field error should name the field, got: %v", err)
	}
}

// TestFileModeRefusesEveryQuickStartVariable: a variable that is neither
// applied nor refused is the worst of the three outcomes. An operator who
// sets RUNPOOL_LOG_LEVEL=debug beside a configuration file gets no debug
// logging and no explanation, and looks for the reason somewhere else.
func TestFileModeRefusesEveryQuickStartVariable(t *testing.T) {
	for _, name := range quickStartTargetVars {
		environ := func(k string) string {
			switch k {
			case EnvConfigFile:
				return "/etc/runpool/config.yaml"
			case name:
				return "debug"
			}
			return ""
		}
		_, err := Load(environ)
		if err == nil {
			t.Errorf("%s beside a configuration file was accepted; it applies to neither", name)
			continue
		}
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error for %s = %q; want it to name the variable", name, err)
		}
	}
}

// TestTheConflictListIsTheThirteenVariables. The refusal and its test both
// iterate quickStartTargetVars, so a variable dropped from the list
// leaves both self-consistent: production stops refusing it beside a
// configuration file and the test stops checking it, in one edit. The
// literals here are the independent statement — a deletion is a diff in
// this constant, reviewed as one.
func TestTheConflictListIsTheThirteenVariables(t *testing.T) {
	want := []string{
		"RUNPOOL_GITHUB_URL", "RUNPOOL_GITHUB_RUNNER_GROUP", "RUNPOOL_GITHUB_TOKEN_FILE",
		"RUNPOOL_CREDENTIAL_FILE_PERMISSIONS",
		"RUNPOOL_HOST_TOPOLOGY", "RUNPOOL_HOST_RESERVE_CPU", "RUNPOOL_HOST_RESERVE_MEMORY",
		"RUNPOOL_HOST_RESERVE_SWAP", "RUNPOOL_HOST_RESERVE_FREE_DISK", "RUNPOOL_TIER",
		"RUNPOOL_PARALLELISM", "RUNPOOL_LOG_LEVEL", "RUNPOOL_NETWORK_PROFILE",
	}
	got := slices.Clone(quickStartTargetVars)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("quickStartTargetVars = %v\nwant %v — RUNPOOL_GITHUB_TOKEN alone is exempt, "+
			"because a file's tokenEnv may name it", quickStartTargetVars, want)
	}
}
