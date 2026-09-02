package credential

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

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

// TestTokenFileRefusesPermissiveModesByDefault: shared-daemon is a supported
// topology, so another uid on the host is a real party. A token file it
// can read is a GitHub credential it can use to mint runners, and reading
// one without comment is not what a package that exists to be careful with
// secrets should do.
func TestTokenFileRefusesPermissiveModesByDefault(t *testing.T) {
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

	// Every one of the six group/other bits is independently forbidden by
	// the default. Exhausting 0000..0077 makes the claim independent of a
	// few representative combinations.
	for extra := os.FileMode(1); extra <= 0o077; extra++ {
		mode := os.FileMode(0o600) | extra
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

// TestEachRungAcceptsExactlyTheBitsItNames exhausts 0600..0677 under every
// rung of the ladder: a mode is accepted exactly when it carries none of the
// bits the rung refuses, so omitting a bit from a mask is a failing change
// rather than an uncovered one. The test process owns the file, so ownership
// is satisfied everywhere and only the bits are under test here.
func TestEachRungAcceptsExactlyTheBitsItNames(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	const secret = "ghs_secret"
	if err := os.WriteFile(path, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	policies := []struct {
		name    config.CredentialFilePermissions
		refused os.FileMode
	}{
		{config.CredentialFilePermissionsOwnerOnly, 0o077},
		{config.CredentialFilePermissionsAllowGroupRead, 0o037},
		{config.CredentialFilePermissionsAllowWorldRead, 0o033},
		{config.CredentialFilePermissionsIgnoreModeAndOwner, 0},
	}
	for _, policy := range policies {
		c := config.Credential{
			ID: "gh", Type: config.CredentialTypeToken, TokenFile: path,
			FilePermissions: policy.name,
		}
		for extra := os.FileMode(0); extra <= 0o077; extra++ {
			mode := os.FileMode(0o600) | extra
			if err := os.Chmod(path, mode); err != nil {
				t.Fatal(err)
			}
			got, err := Token(nil, c)
			wantErr := extra&policy.refused != 0
			if wantErr && err == nil {
				t.Errorf("%s accepted mode %#o; it refuses %#o", policy.name, mode, policy.refused)
			} else if !wantErr && (err != nil || got != secret) {
				t.Errorf("%s: mode %#o = %q, %v; want the token", policy.name, mode, got, err)
			}
			if err != nil && strings.Contains(err.Error(), secret) {
				t.Error("the error leaked the token value")
			}
		}
	}
}

// TestOwnershipIsRequiredOnlyWhenAMiddleRungUsesWidenedBits: every rung must
// preserve owner-only compatibility for an operator-owned 0600 mount. The two
// middle rungs require controller ownership only when group/other bits are in
// use; ignore-mode-and-owner deliberately consults neither.
func TestOwnershipIsRequiredOnlyWhenAMiddleRungUsesWidenedBits(t *testing.T) {
	for _, policy := range []config.CredentialFilePermissions{
		config.CredentialFilePermissionsOwnerOnly,
		config.CredentialFilePermissionsAllowGroupRead,
		config.CredentialFilePermissionsAllowWorldRead,
		config.CredentialFilePermissionsIgnoreModeAndOwner,
	} {
		if err := checkSecretFileMetadata("/run/secrets/token", 0o600, 1001, 0, policy); err != nil {
			t.Errorf("%s rejected a foreign-owned 0600 file: %v", policy, err)
		}
	}

	for _, tc := range []struct {
		rung          config.CredentialFilePermissions
		perm          os.FileMode
		refused       bool
		wantOwnership bool
	}{
		{config.CredentialFilePermissionsOwnerOnly, 0o640, true, false},
		{config.CredentialFilePermissionsAllowGroupRead, 0o640, true, true},
		{config.CredentialFilePermissionsAllowWorldRead, 0o644, true, true},
		{config.CredentialFilePermissionsIgnoreModeAndOwner, 0o666, false, false},
		{config.CredentialFilePermissionsIgnoreModeAndOwner, 0o777, false, false},
	} {
		err := checkSecretFileMetadata("/run/secrets/token", tc.perm, 1001, 0, tc.rung)
		if tc.refused {
			if err == nil {
				t.Errorf("%s accepted foreign-owned mode %#o", tc.rung, tc.perm)
			} else if tc.wantOwnership && (!strings.Contains(err.Error(), "owned by uid 1001") ||
				!strings.Contains(err.Error(), "controller uid 0")) {
				t.Errorf("%s on a foreign-owned %#o file: error = %v; want an ownership refusal", tc.rung, tc.perm, err)
			}
		} else if err != nil {
			t.Errorf("%s on a foreign-owned %#o file: %v; want acceptance", tc.rung, tc.perm, err)
		}
	}
}

func TestOwnerOnlyPreservesOperatorOwnedFileCompatibility(t *testing.T) {
	err := checkSecretFileMetadata(
		"/run/secrets/token", 0o600, 1001, 0,
		config.CredentialFilePermissionsOwnerOnly,
	)
	if err != nil {
		t.Fatalf("operator-owned owner-only file: %v", err)
	}
}

func TestTokenFileRefusesAnUnknownPermissionPolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("ghs_secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Token(nil, config.Credential{
		ID: "gh", TokenFile: path, FilePermissions: "trust-platform",
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported filePermissions") {
		t.Fatalf("unknown policy error = %v", err)
	}
}

// TestARefusalNamesTheNarrowestAcceptingRung: the operator who widens should
// widen by exactly one word, so every refusal says which one.
func TestARefusalNamesTheNarrowestAcceptingRung(t *testing.T) {
	for _, tc := range []struct {
		rung config.CredentialFilePermissions
		perm os.FileMode
		want config.CredentialFilePermissions
	}{
		{config.CredentialFilePermissionsOwnerOnly, 0o640, config.CredentialFilePermissionsAllowGroupRead},
		{config.CredentialFilePermissionsOwnerOnly, 0o644, config.CredentialFilePermissionsAllowWorldRead},
		{config.CredentialFilePermissionsOwnerOnly, 0o755, config.CredentialFilePermissionsIgnoreModeAndOwner},
		{config.CredentialFilePermissionsAllowGroupRead, 0o644, config.CredentialFilePermissionsAllowWorldRead},
		{config.CredentialFilePermissionsAllowWorldRead, 0o664, config.CredentialFilePermissionsIgnoreModeAndOwner},
	} {
		err := checkSecretFileMetadata("/run/secrets/token", tc.perm, 0, 0, tc.rung)
		if err == nil {
			t.Errorf("%s accepted %#o", tc.rung, tc.perm)
			continue
		}
		if !strings.Contains(err.Error(), "set filePermissions: "+string(tc.want)) {
			t.Errorf("%s on %#o: error = %q; want it to name %s", tc.rung, tc.perm, err, tc.want)
		}
	}
	// A foreign-owned file that actually uses widened bits skips the middle
	// rungs: only ignore-mode-and-owner deliberately ignores ownership.
	err := checkSecretFileMetadata("/run/secrets/token", 0o640, 1001, 0, config.CredentialFilePermissionsAllowGroupRead)
	if err == nil || !strings.Contains(err.Error(), "set filePermissions: "+string(config.CredentialFilePermissionsIgnoreModeAndOwner)) {
		t.Errorf("foreign-owned 0640 under allow-group-read: error = %v; want ignore-mode-and-owner named", err)
	}
}

func TestIgnoreModeAndOwnerStillRequiresABoundedRegularFile(t *testing.T) {
	t.Run("device", func(t *testing.T) {
		_, err := Token(nil, config.Credential{
			ID: "gh", TokenFile: "/dev/null",
			FilePermissions: config.CredentialFilePermissionsIgnoreModeAndOwner,
		})
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("device error = %v; want a regular-file refusal", err)
		}
	})

	t.Run("fifo", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "token.pipe")
		if err := syscall.Mkfifo(path, 0o600); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			_, err := Token(nil, config.Credential{
				ID: "gh", TokenFile: path,
				FilePermissions: config.CredentialFilePermissionsIgnoreModeAndOwner,
			})
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), "not a regular file") {
				t.Fatalf("fifo error = %v; want a regular-file refusal", err)
			}
		case <-time.After(time.Second):
			t.Fatal("credential resolution blocked while opening a FIFO")
		}
	})

	t.Run("oversized regular file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "token")
		if err := os.WriteFile(path, bytes.Repeat([]byte("x"), maxSecretFileBytes+1), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := Token(nil, config.Credential{
			ID: "gh", TokenFile: path,
			FilePermissions: config.CredentialFilePermissionsIgnoreModeAndOwner,
		})
		if err == nil || !strings.Contains(err.Error(), "exceeds the") || !strings.Contains(err.Error(), "byte limit") {
			t.Fatalf("oversized-file error = %v; want a size refusal", err)
		}
	})
}

func TestASecretSymlinkIsJudgedByItsRegularFileTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "token")
	if err := os.WriteFile(target, []byte("ghs_secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "current")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	got, err := Token(nil, config.Credential{ID: "gh", TokenFile: link})
	if err != nil || got != "ghs_secret" {
		t.Fatalf("symlink target = %q, %v; want the regular target's token", got, err)
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
		if !strings.Contains(err.Error(), "by group or other") {
			t.Errorf("error = %q", err)
		}
		if strings.Contains(err.Error(), "BEGIN") {
			t.Error("the error carries the key")
		}
	})

	t.Run("a key others can read is accepted only by explicit policy", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "app.pem")
		if err := os.WriteFile(path, []byte(pem), 0o644); err != nil {
			t.Fatal(err)
		}
		c := base
		c.PrivateKeyFile = path
		c.FilePermissions = config.CredentialFilePermissionsAllowWorldRead
		got, err := Resolve(func(string) string { return "" }, c)
		if err != nil {
			t.Fatal(err)
		}
		if got.App == nil || got.App.PrivateKey != pem {
			t.Fatalf("app = %+v; want the key", got.App)
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
