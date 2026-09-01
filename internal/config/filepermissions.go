package config

import (
	"io/fs"
	"strings"
)

// CredentialFilePermissionsPolicy is one rung of the credential-file policy:
// the configured name, the group/other bits it refuses, whether using widened
// read bits depends on controller ownership, and the warning shown by doctor.
// Policy returns this value by copy; callers cannot mutate the ladder itself.
type CredentialFilePermissionsPolicy struct {
	Name CredentialFilePermissions
	// RefusedBits contains only group and other permission bits. Owner bits do
	// not decide which local accounts outside the controller may read the file.
	RefusedBits fs.FileMode
	// RequireControllerOwnerForWidening applies only when the file actually
	// carries group or other bits. This condition keeps the ladder monotonic:
	// every rung still accepts every owner-only-compatible file, including a
	// privileged controller reading an operator-owned 0600 mount.
	RequireControllerOwnerForWidening bool
	// Warning is empty for the default and explains every wider choice.
	Warning string
}

// credentialFilePermissionsLadder is the single policy definition, narrowest
// first. It is deliberately private: validation, credential reading and doctor
// receive copies through Policy instead of sharing a mutable exported slice.
var credentialFilePermissionsLadder = [...]CredentialFilePermissionsPolicy{
	{Name: CredentialFilePermissionsOwnerOnly, RefusedBits: 0o077},
	{Name: CredentialFilePermissionsAllowGroupRead, RefusedBits: 0o037, RequireControllerOwnerForWidening: true,
		Warning: "the policy permits members of the credential file's group to read it"},
	{Name: CredentialFilePermissionsAllowWorldRead, RefusedBits: 0o033, RequireControllerOwnerForWidening: true,
		Warning: "the policy permits any local account to read the credential file"},
	{Name: CredentialFilePermissionsIgnoreModeAndOwner, RefusedBits: 0,
		Warning: "the credential file's mode and owner are not checked: any local account that reaches it may read or replace it"},
}

// Policy resolves a configured name. Empty means the owner-only default so a
// Credential assembled in memory is as safe as one passed through defaults.
func (p CredentialFilePermissions) Policy() (CredentialFilePermissionsPolicy, bool) {
	if p == "" {
		p = CredentialFilePermissionsOwnerOnly
	}
	for _, policy := range credentialFilePermissionsLadder {
		if policy.Name == p {
			return policy, true
		}
	}
	return CredentialFilePermissionsPolicy{}, false
}

// RequiresControllerOwnership reports whether this file is relying on the
// rung's widened group/other permissions. A 0600 file relies on none and keeps
// the compatibility guaranteed by the narrower owner-only rung.
func (p CredentialFilePermissionsPolicy) RequiresControllerOwnership(perm fs.FileMode) bool {
	allowedWidening := perm & 0o077 &^ p.RefusedBits
	return p.RequireControllerOwnerForWidening && allowedWidening != 0
}

// Accepts reports whether the mode and ownership satisfy this policy.
func (p CredentialFilePermissionsPolicy) Accepts(perm fs.FileMode, ownedByController bool) bool {
	if perm&p.RefusedBits != 0 {
		return false
	}
	return !p.RequiresControllerOwnership(perm) || ownedByController
}

// NarrowestCredentialFilePermissions returns the least permissive rung that
// accepts the file. The final rung ignores mode and owner, so one always exists.
func NarrowestCredentialFilePermissions(perm fs.FileMode, ownedByController bool) CredentialFilePermissions {
	for _, policy := range credentialFilePermissionsLadder {
		if policy.Accepts(perm, ownedByController) {
			return policy.Name
		}
	}
	return CredentialFilePermissionsIgnoreModeAndOwner
}

// CredentialFilePermissionsNames lists the accepted values in ladder order.
func CredentialFilePermissionsNames() string {
	names := make([]string, 0, len(credentialFilePermissionsLadder))
	for _, policy := range credentialFilePermissionsLadder {
		names = append(names, string(policy.Name))
	}
	return strings.Join(names, ", ")
}
