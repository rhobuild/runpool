// Package credential resolves configured credential references into
// secret values at the moment of use. Values never enter configuration,
// state or logs; this package is the only place a token value passes
// through, and it hands it to the caller without retaining it.
package credential

import (
	"fmt"
	"os"
	"strings"

	"github.com/rhobuild/runpool/internal/config"
)

// Token resolves a credential's token from its environment-variable or
// file reference. Config validation already guarantees exactly one
// reference is set; this re-checks defensively because a secret path
// must not depend on every caller having validated first. Errors name
// the reference, never the value.
func Token(environ func(string) string, c config.Credential) (string, error) {
	switch {
	case c.TokenEnv != "" && c.TokenFile != "":
		return "", fmt.Errorf("credential %q: tokenEnv and tokenFile are mutually exclusive", c.ID)
	case c.TokenEnv != "":
		v := environ(c.TokenEnv)
		if v == "" {
			return "", fmt.Errorf("credential %q: environment variable %s is empty or unset", c.ID, c.TokenEnv)
		}
		return v, nil
	case c.TokenFile != "":
		if err := checkSecretFileMode(c.TokenFile); err != nil {
			return "", fmt.Errorf("credential %q: %w", c.ID, err)
		}
		data, err := os.ReadFile(c.TokenFile)
		if err != nil {
			return "", fmt.Errorf("credential %q: %w", c.ID, err)
		}
		// Mounted secret files conventionally end with a newline; tokens
		// never contain whitespace, so trimming is safe and expected.
		v := strings.TrimSpace(string(data))
		if v == "" {
			return "", fmt.Errorf("credential %q: file %s is empty", c.ID, c.TokenFile)
		}
		return v, nil
	default:
		return "", fmt.Errorf("credential %q: no token reference configured", c.ID)
	}
}

// checkSecretFileMode refuses a secret file other local users can read.
// `shared-daemon` is a supported topology, so "another uid on this host"
// is a real party, not a hypothetical one — and a GitHub token is the
// credential that mints runners. This is the one function whose whole
// purpose is to be careful with a secret, so the check belongs here
// rather than in each caller.
//
// A symlink is resolved before the check: the mode that matters is the
// file's own, not the link's, and Lstat on a link reports 0777.
func checkSecretFileMode(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("secret file %s is a directory", path)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("secret file %s is readable by group or other (mode %#o); "+
			"restrict it to the controller's own user with chmod 600", path, perm)
	}
	return nil
}

// Secret is the material one credential names, resolved. Exactly one of
// its members is set, which is the same thing the credential's type says
// — carried as a value so a caller branches on what it holds rather than
// on a string it has to interpret.
type Secret struct {
	// Token authenticates as the person who minted it.
	Token string
	// App authenticates as an installation of a GitHub App. Nil unless
	// the credential is one.
	App *AppKey
}

// AppKey is a GitHub App installation's identity and its private key. The
// ids are not secret and the key is; they travel together because the
// provider client cannot mint an installation token without all three.
type AppKey struct {
	ClientID       string
	InstallationID int64
	PrivateKey     string
}

// Resolve turns a credential reference into the secret it names, at the
// moment of use. It is the one door: every kind of credential passes
// through here, so the file-mode rule and the "never log the value" rule
// have one place to hold rather than one per kind.
func Resolve(environ func(string) string, c config.Credential) (Secret, error) {
	switch c.Type {
	case config.CredentialTypeGitHubApp:
		key, err := appKey(environ, c)
		if err != nil {
			return Secret{}, err
		}
		return Secret{App: key}, nil
	default:
		// An unset type is a token: the field defaults to it, and
		// validation refuses anything else this does not name.
		token, err := Token(environ, c)
		if err != nil {
			return Secret{}, err
		}
		return Secret{Token: token}, nil
	}
}

func appKey(environ func(string) string, c config.Credential) (*AppKey, error) {
	var pem string
	switch {
	case c.PrivateKeyEnv != "" && c.PrivateKeyFile != "":
		return nil, fmt.Errorf("credential %q: privateKeyEnv and privateKeyFile are mutually exclusive", c.ID)
	case c.PrivateKeyEnv != "":
		// Only the surrounding whitespace, on both paths: a PEM is
		// multi-line and the lines between the markers are the key.
		pem = strings.TrimSpace(environ(c.PrivateKeyEnv))
		if pem == "" {
			return nil, fmt.Errorf("credential %q: environment variable %s is empty or unset",
				c.ID, c.PrivateKeyEnv)
		}
	case c.PrivateKeyFile != "":
		if err := checkSecretFileMode(c.PrivateKeyFile); err != nil {
			return nil, fmt.Errorf("credential %q: %w", c.ID, err)
		}
		data, err := os.ReadFile(c.PrivateKeyFile)
		if err != nil {
			return nil, fmt.Errorf("credential %q: %w", c.ID, err)
		}
		pem = strings.TrimSpace(string(data))
		if pem == "" {
			return nil, fmt.Errorf("credential %q: file %s is empty", c.ID, c.PrivateKeyFile)
		}
	default:
		return nil, fmt.Errorf("credential %q: no private key reference configured", c.ID)
	}
	if c.ClientID == "" || c.InstallationID <= 0 {
		return nil, fmt.Errorf("credential %q: a GitHub App credential needs a client id and an installation id", c.ID)
	}
	return &AppKey{ClientID: c.ClientID, InstallationID: c.InstallationID, PrivateKey: pem}, nil
}
