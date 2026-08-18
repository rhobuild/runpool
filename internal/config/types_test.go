package config

import (
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestParseByteSize(t *testing.T) {
	valid := map[string]int64{
		"0B":      0,
		"512B":    512,
		"4KiB":    4 << 10,
		"1536MiB": 1536 << 20,
		"150GiB":  150 << 30,
		"2TiB":    2 << 40,
	}
	for in, want := range valid {
		got, err := ParseByteSize(in)
		if err != nil || int64(got) != want {
			t.Errorf("ParseByteSize(%q) = %d, %v; want %d", in, got, err, want)
		}
		if s := got.String(); s != in {
			t.Errorf("ByteSize(%q).String() = %q; want round-trip", in, s)
		}
	}
	for _, in := range []string{"", "150", "4GB", "1.5GiB", "GiB", "-1B", "+4GiB", "4 GiB", "99999999999TiB"} {
		if _, err := ParseByteSize(in); err == nil {
			t.Errorf("ParseByteSize(%q) succeeded; want error", in)
		}
	}
}

func TestParseCPUQuantity(t *testing.T) {
	valid := map[string]int64{
		"0":    0,
		"2":    2 * nanoPerCPU,
		"0.5":  nanoPerCPU / 2,
		"1.25": 1_250_000_000,
	}
	for in, want := range valid {
		got, err := ParseCPUQuantity(in)
		if err != nil || int64(got) != want {
			t.Errorf("ParseCPUQuantity(%q) = %d, %v; want %d", in, got, err, want)
		}
		if s := got.String(); s != in {
			t.Errorf("CPUQuantity(%q).String() = %q; want round-trip", in, s)
		}
	}
	for _, in := range []string{"", ".5", "1.", "1.1234567890", "2,5", "-1", "1e3", "1234567"} {
		if _, err := ParseCPUQuantity(in); err == nil {
			t.Errorf("ParseCPUQuantity(%q) succeeded; want error", in)
		}
	}
}

func TestDuration(t *testing.T) {
	d, err := ParseDuration("720h")
	if err != nil || time.Duration(d) != 720*time.Hour {
		t.Fatalf("ParseDuration(720h) = %v, %v", d, err)
	}
	if s := d.String(); s != "720h" {
		t.Errorf("Duration(720h).String() = %q", s)
	}
	if s := Duration(90 * time.Minute).String(); s != "1h30m" {
		t.Errorf("Duration(90m).String() = %q; want 1h30m", s)
	}
	for _, in := range []string{"", "3d", "-1h", "720"} {
		if _, err := ParseDuration(in); err == nil {
			t.Errorf("ParseDuration(%q) succeeded; want error", in)
		}
	}
}

func TestParseCIDR(t *testing.T) {
	for _, in := range []string{"10.0.0.0/8", "192.168.1.0/24", "fd00::/8"} {
		c, err := ParseCIDR(in)
		if err != nil {
			t.Errorf("ParseCIDR(%q): %v", in, err)
		}
		if c.Prefix.String() != in {
			t.Errorf("ParseCIDR(%q).String() = %q", in, c.Prefix)
		}
	}
	for _, in := range []string{"10.0.0.1/8", "10.0.0.0", "10.0.0.0/33", "example.com/8"} {
		if _, err := ParseCIDR(in); err == nil {
			t.Errorf("ParseCIDR(%q) succeeded; want error", in)
		}
	}
}

func TestParseTargetURL(t *testing.T) {
	repo, err := ParseTargetURL("https://github.com/acme/app")
	if err != nil || repo.Scope != ScopeRepository || repo.Owner != "acme" || repo.Repository != "app" {
		t.Fatalf("repository parse = %+v, %v", repo, err)
	}
	org, err := ParseTargetURL("https://github.com/acme/")
	if err != nil || org.Scope != ScopeOrganization || org.CanonicalURL != "https://github.com/acme" {
		t.Fatalf("organization parse = %+v, %v", org, err)
	}
	invalid := []string{
		"",
		"http://github.com/acme",
		"https://github.com/acme/app/tree/main",
		"https://github.com/acme?tab=repositories",
		"https://user@github.com/acme",
		"https://github.com/",
	}
	for _, in := range invalid {
		if _, err := ParseTargetURL(in); err == nil {
			t.Errorf("ParseTargetURL(%q) succeeded; want error", in)
		}
	}
}

// TestParseTargetURLCarriesTheHost: what serves the protocol is a
// question the provider answers, so the host is carried rather than
// checked against a name. An Enterprise Server and a data-residency host
// speak the endpoints github.com speaks; refusing them here would refuse
// them for being unfamiliar rather than for not working.
func TestParseTargetURLCarriesTheHost(t *testing.T) {
	for _, tc := range []struct {
		in    string
		scope TargetScope
		canon string
	}{
		{"https://ghe.example.com/acme", ScopeOrganization, "https://ghe.example.com/acme"},
		{"https://ghe.example.com/acme/app", ScopeRepository, "https://ghe.example.com/acme/app"},
		{"https://acme.ghe.com/team", ScopeOrganization, "https://acme.ghe.com/team"},
		{"https://GitHub.com/Acme", ScopeOrganization, "https://github.com/Acme"},
	} {
		got, err := ParseTargetURL(tc.in)
		if err != nil {
			t.Errorf("ParseTargetURL(%q): %v", tc.in, err)
			continue
		}
		if got.Scope != tc.scope || got.CanonicalURL != tc.canon {
			t.Errorf("ParseTargetURL(%q) = %s %q; want %s %q",
				tc.in, got.Scope, got.CanonicalURL, tc.scope, tc.canon)
		}
	}
}

// TestParseTargetURLReadsAnEnterprise: /enterprises/<name> has two path
// segments that both satisfy the owner and repository patterns, so
// without a case of its own it is not refused — it is silently understood
// as a repository named after the enterprise, owned by "enterprises".
func TestParseTargetURLReadsAnEnterprise(t *testing.T) {
	got, err := ParseTargetURL("https://github.com/enterprises/acme")
	if err != nil {
		t.Fatalf("ParseTargetURL: %v", err)
	}
	if got.Scope != ScopeEnterprise {
		t.Errorf("scope = %s, want %s", got.Scope, ScopeEnterprise)
	}
	if got.Owner != "acme" || got.Repository != "" {
		t.Errorf("ref = %+v; want the enterprise named, and no repository", got)
	}
	if got.CanonicalURL != "https://github.com/enterprises/acme" {
		t.Errorf("canonical = %q", got.CanonicalURL)
	}
}

// TestCIDRYAMLRoundTrip: the CIDR type is the one config value that reaches
// the egress ruleset, so both directions have to hold. Unmarshalling is
// where a bad prefix must fail; marshalling is what `config effective`
// prints, and a value that does not survive the round trip means the
// effective document is not loadable.
func TestCIDRYAMLRoundTrip(t *testing.T) {
	type doc struct {
		Nets []CIDR `yaml:"nets"`
	}

	var in doc
	if err := yaml.Unmarshal([]byte("nets:\n  - 10.0.0.0/8\n  - 169.254.0.0/16\n"), &in); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(in.Nets) != 2 || in.Nets[0].String() != "10.0.0.0/8" {
		t.Fatalf("parsed = %v", in.Nets)
	}

	out, err := yaml.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back doc
	if err := yaml.Unmarshal(out, &back); err != nil {
		t.Fatalf("re-unmarshal of the marshalled form: %v", err)
	}
	if len(back.Nets) != len(in.Nets) || back.Nets[0] != in.Nets[0] || back.Nets[1] != in.Nets[1] {
		t.Errorf("round trip changed the value: %v -> %s -> %v", in.Nets, out, back.Nets)
	}

	// Host bits set is the error worth naming: it is the typo that would
	// otherwise silently widen or narrow a rule.
	for _, bad := range []string{"10.0.0.1/8", "not-a-cidr", "10.0.0.0/33"} {
		if err := yaml.Unmarshal([]byte("nets:\n  - "+bad+"\n"), &doc{}); err == nil {
			t.Errorf("unmarshal accepted %q", bad)
		}
	}

	// A null list entry yields no entry at all rather than a zero CIDR, so
	// there is no path by which an empty line becomes a prefix. Worth
	// pinning: the zero Prefix is invalid, but 0.0.0.0/0 is what an empty
	// value would have to mean if one ever leaked through.
	var nul doc
	if err := yaml.Unmarshal([]byte("nets:\n  -\n"), &nul); err != nil {
		t.Fatalf("a null entry failed to unmarshal: %v", err)
	}
	if len(nul.Nets) != 0 {
		t.Errorf("null entry produced %v; want no entry", nul.Nets)
	}
	// And an invalid prefix reaching the config by any other route is
	// refused by validation, not silently rendered into the ruleset.
	cfg := validConfig()
	cfg.Network.AllowPrivateCIDRs = []CIDR{{}}
	if err := Validate(cfg); err == nil {
		t.Error("an invalid allowPrivateCIDRs entry passed validation")
	}
}

// TestRetentionWindow: zero is a decision — keep every record — and it
// has to survive the unwrapping. Folding it in with nil, the way a
// `LeaseHistory == nil || *LeaseHistory == 0` reading would, turns an
// operator's "keep forever" into the ninety-day default and starts
// deleting records they asked to be kept.
func TestRetentionWindow(t *testing.T) {
	keep := Duration(0)
	week := Duration(7 * 24 * time.Hour)
	for name, tc := range map[string]struct {
		in   Retention
		want time.Duration
	}{
		"unset takes the default": {Retention{}, time.Duration(DefaultLeaseHistory)},
		"zero keeps every record": {Retention{LeaseHistory: &keep}, 0},
		"configured is honoured":  {Retention{LeaseHistory: &week}, 7 * 24 * time.Hour},
	} {
		if got := tc.in.Window(); got != tc.want {
			t.Errorf("%s: Window() = %s; want %s", name, got, tc.want)
		}
	}
}

// TestTierImage: a tier's own image replaces the shipped one for that
// tier alone, and a tier that names none runs what the build ships. The
// binding that launches and the status document that reports both apply
// this method, so this is the one place the rule is pinned.
func TestTierImage(t *testing.T) {
	const shipped = "ghcr.io/rhobuild/runpool/capsule@sha256:" +
		"1111111111111111111111111111111111111111111111111111111111111111"
	const operators = "ghcr.io/acme/capsule@sha256:" +
		"2222222222222222222222222222222222222222222222222222222222222222"

	if got := (Tier{ID: "standard"}).Image(shipped); got != shipped {
		t.Errorf("a tier naming no image runs %q, want the shipped capsule", got)
	}
	if got := (Tier{ID: "heavy", CapsuleImage: operators}).Image(shipped); got != operators {
		t.Errorf("a tier naming an image runs %q, want its own", got)
	}
}

// TestParseTargetURLTranslatesTheOrgsAddress: the address a browser
// shows for an organization has two segments that both satisfy the
// owner and repository patterns, so without its own case it would be
// read as a repository named after the organization and owned by
// "orgs" — an owner that cannot exist, discovered only at the provider.
func TestParseTargetURLTranslatesTheOrgsAddress(t *testing.T) {
	got, err := ParseTargetURL("https://github.com/orgs/acme")
	if err != nil {
		t.Fatal(err)
	}
	if got.Scope != ScopeOrganization || got.Owner != "acme" ||
		got.CanonicalURL != "https://github.com/acme" {
		t.Errorf("orgs address = %s %q %q; want the organization it names",
			got.Scope, got.Owner, got.CanonicalURL)
	}
}

// TestParseTargetURLTrimsTheCloneSuffix: a clone URL's .git names no
// repository — the API does not address one — so keeping it turns a
// recognisable paste into a 404 against the provider long after the
// operator has stopped looking at the URL they wrote.
func TestParseTargetURLTrimsTheCloneSuffix(t *testing.T) {
	got, err := ParseTargetURL("https://github.com/acme/app.git")
	if err != nil {
		t.Fatal(err)
	}
	if got.Repository != "app" || got.CanonicalURL != "https://github.com/acme/app" {
		t.Errorf("clone URL = %q %q; want the repository it names", got.Repository, got.CanonicalURL)
	}
	// The suffix alone is not a name.
	if _, err := ParseTargetURL("https://github.com/acme/.git"); err == nil {
		t.Error("a bare .git parsed as a repository")
	}
}

// TestParseTargetURLRefusesReservedRoutes: every entry in the list 404s
// as an account, so the refusal shadows nobody — and without it a pasted
// settings or marketplace address reads as an owner and fails much later
// with the provider's answer instead of this one.
func TestParseTargetURLRefusesReservedRoutes(t *testing.T) {
	for _, in := range []string{
		"https://github.com/settings/profile",
		"https://github.com/marketplace",
		"https://github.com/notifications",
		"https://github.com/sponsors/acme",
		"https://github.com/topics/ci",
		"https://github.com/apps/runpool",
	} {
		_, err := ParseTargetURL(in)
		if err == nil {
			t.Errorf("ParseTargetURL(%q) succeeded; the route is the provider's, not an owner's", in)
			continue
		}
		if !strings.Contains(err.Error(), "reserved route") {
			t.Errorf("ParseTargetURL(%q) = %v; want the refusal to say why", in, err)
		}
	}
}
