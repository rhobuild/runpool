package credential

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rhobuild/runpool/internal/config"
)

func TestToken(t *testing.T) {
	secret := "tok-abc123-secret"
	dir := t.TempDir()
	file := filepath.Join(dir, "token")
	if err := os.WriteFile(file, []byte(secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	environ := func(name string) string {
		if name == "MY_TOKEN" {
			return secret
		}
		return ""
	}

	cases := map[string]struct {
		cred config.Credential
		want string
	}{
		"from env":            {config.Credential{ID: "c", TokenEnv: "MY_TOKEN"}, secret},
		"from file trims eol": {config.Credential{ID: "c", TokenFile: file}, secret},
		"unset env":           {config.Credential{ID: "c", TokenEnv: "OTHER"}, ""},
		"missing file":        {config.Credential{ID: "c", TokenFile: file + ".missing"}, ""},
		"empty file":          {config.Credential{ID: "c", TokenFile: empty}, ""},
		"both references":     {config.Credential{ID: "c", TokenEnv: "MY_TOKEN", TokenFile: file}, ""},
		"no reference":        {config.Credential{ID: "c"}, ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := Token(environ, tc.cred)
			if tc.want == "" {
				if err == nil {
					t.Fatal("want error")
				}
				// A resolution error must never leak the secret value.
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("error leaks the secret: %v", err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("Token = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

// TestTokenFileRefusesPermissiveModes: shared-daemon is a supported
// topology, so another uid on the host is a real party. A token file it
// can read is a GitHub credential it can use to mint runners, and reading
// one without comment is not what a package that exists to be careful with
// secrets should do.
func TestTokenFileRefusesPermissiveModes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte("ghs_secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := config.Credential{ID: "gh", Type: config.CredentialTypeToken, TokenFile: path}

	got, err := Token(nil, c)
	if err != nil {
		t.Fatalf("0600 token file rejected: %v", err)
	}
	if got != "ghs_secret" {
		t.Errorf("token = %q; want the trimmed file contents", got)
	}

	for _, mode := range []os.FileMode{0o640, 0o604, 0o644, 0o666} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		_, err := Token(nil, c)
		if err == nil {
			t.Errorf("mode %#o was accepted; group or other can read the token", mode)
			continue
		}
		if strings.Contains(err.Error(), "ghs_secret") {
			t.Error("the error leaked the token value")
		}
	}
}

// TestResolveAGitHubAppCredential: the key is the longest-lived secret a
// deployment holds, so it takes the same references and the same
// owner-only file rule a token does, and the ids travel with it because
// the provider cannot mint an installation token without all three.
func TestResolveAGitHubAppCredential(t *testing.T) {
	const pem = "-----BEGIN RSA PRIVATE KEY-----\nMIIB\nkey\n-----END RSA PRIVATE KEY-----"
	base := config.Credential{
		ID: "app", Type: config.CredentialTypeGitHubApp,
		ClientID: "Iv1.abc", InstallationID: 42,
	}

	t.Run("from the environment", func(t *testing.T) {
		c := base
		c.PrivateKeyEnv = "APP_KEY"
		got, err := Resolve(func(k string) string {
			if k == "APP_KEY" {
				return pem + "\n"
			}
			return ""
		}, c)
		if err != nil {
			t.Fatal(err)
		}
		if got.Token != "" {
			t.Error("an app credential resolved a token")
		}
		if got.App == nil || got.App.PrivateKey != pem {
			t.Fatalf("app = %+v; want the key with its surrounding whitespace trimmed", got.App)
		}
		if got.App.ClientID != "Iv1.abc" || got.App.InstallationID != 42 {
			t.Errorf("app identity = %+v", got.App)
		}
	})

	t.Run("from a file the owner alone can read", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "app.pem")
		if err := os.WriteFile(path, []byte(pem+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		c := base
		c.PrivateKeyFile = path
		got, err := Resolve(func(string) string { return "" }, c)
		if err != nil {
			t.Fatal(err)
		}
		if got.App == nil || got.App.PrivateKey != pem {
			t.Fatalf("app = %+v; want the multi-line key intact", got.App)
		}
	})

	t.Run("a key others can read is refused", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "app.pem")
		if err := os.WriteFile(path, []byte(pem), 0o644); err != nil {
			t.Fatal(err)
		}
		c := base
		c.PrivateKeyFile = path
		_, err := Resolve(func(string) string { return "" }, c)
		if err == nil {
			t.Fatal("a world-readable private key was accepted")
		}
		if !strings.Contains(err.Error(), "readable by group or other") {
			t.Errorf("error = %q", err)
		}
		if strings.Contains(err.Error(), "BEGIN") {
			t.Error("the error carries the key")
		}
	})

	t.Run("an empty reference is refused", func(t *testing.T) {
		c := base
		c.PrivateKeyEnv = "APP_KEY"
		if _, err := Resolve(func(string) string { return "" }, c); err == nil {
			t.Fatal("an unset key variable was accepted")
		}
	})
}
