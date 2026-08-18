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
// file reference. Errors name the reference, never the value.
func Token(environ func(string) string, c config.Credential) (string, error) {
	return readSecret(environ, c.ID, secretRef{
		kind: "token", envField: "tokenEnv", fileField: "tokenFile",
		envName: c.TokenEnv, filePath: c.TokenFile,
	})
}

// secretRef names one env-xor-file secret reference: which fields carry
// it, for the errors, and what they hold, for the resolution.
type secretRef struct {
	kind      string // what the reference names, for the no-reference error
	envField  string // the configuration field naming the variable
	fileField string // the configuration field naming the file
	envName   string
	filePath  string
}

// readSecret is the one ladder every secret reference descends:
// exactly-one enforcement, the file-mode refusal, the read, the trim
// and the emptiness check. Config validation already guarantees exactly
// one reference is set; this re-checks defensively because a secret
// path must not depend on every caller having validated first. One
// ladder rather than one per kind, so the next credential type costs a
// call here and cannot fork the rules.
//
// Both paths trim surrounding whitespace: mounted secret files
// conventionally end with a newline, a PEM's inner lines are untouched
// by TrimSpace, and neither a token nor a key begins or ends with
// meaningful whitespace.
func readSecret(environ func(string) string, credID string, ref secretRef) (string, error) {
	switch {
	case ref.envName != "" && ref.filePath != "":
		return "", fmt.Errorf("credential %q: %s and %s are mutually exclusive", credID, ref.envField, ref.fileField)
	case ref.envName != "":
		v := strings.TrimSpace(environ(ref.envName))
		if v == "" {
			return "", fmt.Errorf("credential %q: environment variable %s is empty or unset", credID, ref.envName)
		}
		return v, nil
	case ref.filePath != "":
		if err := checkSecretFileMode(ref.filePath); err != nil {
			return "", fmt.Errorf("credential %q: %w", credID, err)
		}
		data, err := os.ReadFile(ref.filePath)
		if err != nil {
			return "", fmt.Errorf("credential %q: %w", credID, err)
		}
		v := strings.TrimSpace(string(data))
		if v == "" {
			return "", fmt.Errorf("credential %q: file %s is empty", credID, ref.filePath)
		}
		return v, nil
	default:
		return "", fmt.Errorf("credential %q: no %s reference configured", credID, ref.kind)
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
	pem, err := readSecret(environ, c.ID, secretRef{
		kind: "private key", envField: "privateKeyEnv", fileField: "privateKeyFile",
		envName: c.PrivateKeyEnv, filePath: c.PrivateKeyFile,
	})
	if err != nil {
		return nil, err
	}
	if c.ClientID == "" || c.InstallationID <= 0 {
		return nil, fmt.Errorf("credential %q: a GitHub App credential needs a client id and an installation id", c.ID)
	}
	return &AppKey{ClientID: c.ClientID, InstallationID: c.InstallationID, PrivateKey: pem}, nil
}
