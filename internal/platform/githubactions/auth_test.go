package githubactions

import (
	"strings"
	"testing"

	"github.com/rhobuild/runpool/internal/credential"
)

// An incomplete GitHub App credential is refused while the client is
// being built. It is worth pinning: the upstream constructor validates
// the config URL and nothing about the credential, so without this the
// branch would be observable only as a failed API call — and an App
// credential that silently took the token path would authenticate as
// nothing at all, since an empty personal access token is accepted here
// and refused by the provider.
func TestAnIncompleteAppCredentialIsRefusedAtBuild(t *testing.T) {
	for name, app := range map[string]*credential.AppKey{
		"no client id":       {InstallationID: 42, PrivateKey: "key"},
		"no installation id": {ClientID: "Iv1.abc", PrivateKey: "key"},
		"no private key":     {ClientID: "Iv1.abc", InstallationID: 42},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NewClient(ClientConfig{
				ConfigURL:  "https://github.com/acme/app",
				Version:    "test",
				Credential: credential.Secret{App: app},
			})
			if err == nil {
				t.Fatal("an incomplete app credential built a client")
			}
			if !strings.Contains(err.Error(), "github app credential") {
				t.Errorf("error = %q; want the app path named", err)
			}
		})
	}
}

// A complete one builds, so the refusal above is about what is missing
// rather than about the App path itself.
func TestACompleteAppCredentialBuilds(t *testing.T) {
	c, err := NewClient(ClientConfig{
		ConfigURL: "https://github.com/acme/app",
		Version:   "test",
		Credential: credential.Secret{App: &credential.AppKey{
			ClientID:       "Iv1.abc",
			InstallationID: 42,
			PrivateKey:     "-----BEGIN RSA PRIVATE KEY-----\nk\n-----END RSA PRIVATE KEY-----",
		}},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if c == nil {
		t.Fatal("no client")
	}
}

// A token credential still takes the token constructor, which accepts the
// value without reading it: nothing about a token is checkable offline.
func TestATokenCredentialStillBuilds(t *testing.T) {
	c, err := NewClient(ClientConfig{
		ConfigURL:  "https://github.com/acme/app",
		Version:    "test",
		Credential: credential.Secret{Token: "t0ken"},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if c == nil {
		t.Fatal("no client")
	}
}

// An empty personal access token is refused while the client is built,
// symmetric with the incomplete-App refusal above and for the same
// reason: the upstream constructor validates the config URL and nothing
// about the credential, so the empty token would authenticate as nobody
// and surface as the provider's authorization error instead of as a
// statement about the configuration.
func TestAnEmptyTokenIsRefusedAtBuild(t *testing.T) {
	_, err := NewClient(ClientConfig{
		ConfigURL:  "https://github.com/acme/app",
		Credential: credential.Secret{},
		Version:    "test",
	})
	if err == nil {
		t.Fatal("an empty token built a client")
	}
	if !strings.Contains(err.Error(), "token is empty") {
		t.Errorf("error = %v; want it to name the empty token", err)
	}
}

// TestIsHostedDomain mirrors the upstream client's own classification,
// which is what decides whether the API is addressed as api.<host> or
// as <host>/api/v3. The predicate feeds visibility only — the startup
// log and the doctor's boundary warning — never admission.
func TestIsHostedDomain(t *testing.T) {
	hosted := []string{"github.com", "GitHub.com", "www.github.com", "github.localhost", "acme.ghe.com", "eu.GHE.com"}
	for _, h := range hosted {
		if !IsHostedDomain(h) {
			t.Errorf("IsHostedDomain(%q) = false; GitHub operates it", h)
		}
	}
	notHosted := []string{"gihub.com", "ghe.com", "ghes.internal", "github.com.evil.example", "api.github.com.example"}
	for _, h := range notHosted {
		if IsHostedDomain(h) {
			t.Errorf("IsHostedDomain(%q) = true; GitHub does not operate it", h)
		}
	}
}
