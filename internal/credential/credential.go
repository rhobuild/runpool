// Package credential resolves configured credential references into
// secret values at the moment of use. Values never enter configuration,
// state or logs; this package is the only place a token value passes
// through, and it hands it to the caller without retaining it.
package credential

import (
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"

	"github.com/rhobuild/runpool/internal/config"
)

// maxSecretFileBytes bounds what a broken mount or a writer can make the
// controller retain while resolving one credential. Tokens and PEM keys are
// many orders of magnitude smaller; one MiB leaves ample room without making
// a credential path an unbounded input.
const maxSecretFileBytes = 1 << 20

// Token resolves a credential's token from its environment-variable or
// file reference. Errors name the reference, never the value.
func Token(environ func(string) string, c config.Credential) (string, error) {
	return readSecret(environ, c.ID, secretRef{
		kind: "token", envField: "tokenEnv", fileField: "tokenFile",
		envName: c.TokenEnv, filePath: c.TokenFile, filePermissions: c.FilePermissions,
	})
}

// secretRef names one env-xor-file secret reference: which fields carry
// it, for the errors, and what they hold, for the resolution.
type secretRef struct {
	kind            string // what the reference names, for the no-reference error
	envField        string // the configuration field naming the variable
	fileField       string // the configuration field naming the file
	envName         string
	filePath        string
	filePermissions config.CredentialFilePermissions
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
		data, err := readSecretFile(ref.filePath, ref.filePermissions)
		if err != nil {
			return "", fmt.Errorf("credential %q: read %s: %w", credID, ref.fileField, err)
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

// readSecretFile applies the credential's explicit local-read policy and reads
// through the same descriptor it inspected. A path can be replaced between
// two syscalls; opening once ensures the bytes come from the inode whose mode
// and owner passed the policy.
//
// `shared-daemon` is a supported topology, so "another uid on this host"
// is a real party, not a hypothetical one — and a GitHub token is the
// credential that mints runners. Owner-only is therefore the default. A
// deployment may stand on a wider named rung, down to ignore-mode-and-owner
// when it deliberately delegates those POSIX decisions to its platform. Every
// rung still accepts only a bounded regular file. This is the one function
// whose whole purpose is to be careful with a secret, so the check belongs here.
//
// A symlink is resolved before the check: the mode that matters is the
// file's own, not the link's, and Lstat on a link reports 0777.
func readSecretFile(path string, policy config.CredentialFilePermissions) ([]byte, error) {
	// Non-blocking matters only for special files and lets us reject a FIFO
	// without hanging before f.Stat can identify it. It has no effect on a
	// regular file.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("secret file %s is not a regular file", path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, fmt.Errorf("secret file %s ownership cannot be determined", path)
	}
	if err := checkSecretFileMetadata(path, info.Mode().Perm(), stat.Uid, uint32(os.Geteuid()), policy); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, maxSecretFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxSecretFileBytes {
		return nil, fmt.Errorf("secret file %s exceeds the %d-byte limit", path, maxSecretFileBytes)
	}
	return data, nil
}

// checkSecretFileMetadata keeps the ownership and bit policy in one pure
// decision that tests can exercise for UIDs the test process cannot chown a
// file to. A middle rung requires ownership only when the file actually uses
// widened group/other bits: a Unix owner could otherwise change those bits
// later. That condition preserves a real ladder, because every rung accepts
// every file accepted by owner-only, including an operator-owned 0600 mount.
func checkSecretFileMetadata(path string, perm os.FileMode, ownerUID, effectiveUID uint32, name config.CredentialFilePermissions) error {
	policy, ok := name.Policy()
	if !ok {
		return fmt.Errorf("secret file %s has unsupported filePermissions policy %q; use one of %s",
			path, name, config.CredentialFilePermissionsNames())
	}
	owned := ownerUID == effectiveUID
	narrowest := config.NarrowestCredentialFilePermissions(perm, owned)
	if refused := perm & policy.RefusedBits; refused != 0 {
		return fmt.Errorf("secret file %s is %s by group or other (mode %#o), which filePermissions: %s refuses; "+
			"chmod 600, or set filePermissions: %s, the narrowest policy that accepts this file",
			path, describeGroupOtherBits(refused), perm, policy.Name, narrowest)
	}
	if policy.RequiresControllerOwnership(perm) && !owned {
		return fmt.Errorf("secret file %s is owned by uid %d, not the controller uid %d; "+
			"filePermissions: %s trusts widened group or other permissions only on a file the controller owns: "+
			"chown it, restrict it to mode 0600, or set filePermissions: %s, the narrowest policy that accepts this file",
			path, ownerUID, effectiveUID, policy.Name, narrowest)
	}
	return nil
}

// describeGroupOtherBits names the bit classes set among the refused group
// and other bits, so the message says what another account could do rather
// than printing a mask to be decoded.
func describeGroupOtherBits(bits os.FileMode) string {
	var words []string
	if bits&0o044 != 0 {
		words = append(words, "readable")
	}
	if bits&0o022 != 0 {
		words = append(words, "writable")
	}
	if bits&0o011 != 0 {
		words = append(words, "executable")
	}
	return strings.Join(words, ", ")
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
		envName: c.PrivateKeyEnv, filePath: c.PrivateKeyFile, filePermissions: c.FilePermissions,
	})
	if err != nil {
		return nil, err
	}
	if c.ClientID == "" || c.InstallationID <= 0 {
		return nil, fmt.Errorf("credential %q: a GitHub App credential needs a client id and an installation id", c.ID)
	}
	return &AppKey{ClientID: c.ClientID, InstallationID: c.InstallationID, PrivateKey: pem}, nil
}
