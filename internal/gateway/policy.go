package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/rhobuild/runpool/internal/egress"
)

// MaxPolicyBytes bounds a policy document. The real one is a few
// kilobytes of CIDRs; anything larger is a mistake or an attempt.
const MaxPolicyBytes = 1 << 20

// ParsePolicy decodes and validates a policy document.
func ParsePolicy(raw string) (egress.Policy, error) {
	var p egress.Policy
	if raw == "" {
		return p, errors.New("policy is empty")
	}
	if len(raw) > MaxPolicyBytes {
		return p, fmt.Errorf("policy exceeds %d bytes", MaxPolicyBytes)
	}
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return p, err
	}
	return p, p.Validate()
}

// PolicyStore holds the compiled policy and reloads it when the file
// changes. A reload arrives as a separate exec into this container, so
// the file is the only channel into the running relay; re-reading it on
// change means the new policy binds the very next connection while
// established ones are unaffected.
//
// A policy that cannot be read or compiled is not replaced by an empty
// one — Current returns the error and every dial refuses, because a
// gateway with no policy must deny, not allow.
type PolicyStore struct {
	Path string

	mu      sync.Mutex
	decider *egress.Decider
	stamp   string
	// generation counts installed policies. A pooled HTTP connection was
	// authorised by whichever policy was in force when it was dialled, and
	// http.Transport reuses it without consulting the dialer again, so the
	// relay needs to know the policy moved in order to drop those
	// connections. A counter says that; comparing deciders does not.
	generation uint64
}

// Current returns the compiled policy in force, reloading it if the
// file changed since the last read.
func (s *PolicyStore) Current() (*egress.Decider, error) {
	info, err := os.Stat(s.Path)
	if err != nil {
		return nil, err
	}
	stamp := fmt.Sprintf("%d/%d", info.ModTime().UnixNano(), info.Size())

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.decider != nil && s.stamp == stamp {
		return s.decider, nil
	}
	raw, err := os.ReadFile(s.Path)
	if err != nil {
		return nil, err
	}
	policy, err := ParsePolicy(string(raw))
	if err != nil {
		return nil, err
	}
	decider, err := policy.Compile()
	if err != nil {
		return nil, err
	}
	s.decider, s.stamp = decider, stamp
	s.generation++
	return decider, nil
}

// Generation is how many policies this store has installed. It changes
// only when a new one takes effect, so a caller holding connections
// authorised under the previous policy can tell that they are stale.
func (s *PolicyStore) Generation() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.generation
}

// Reload installs a new allow/deny set on a live gateway, given the
// sets on a reader. The subnets already in force are kept: which two
// networks this gateway bridges is a fact of its own creation, not
// something a later discovery pass may redefine.
//
// The handover is transactional on each half — one kernel restore, one
// rename — but not across them, and the window between is deliberately
// the *stricter* of the two policies: the firewall is installed first,
// so during the window the kernel already enforces the new denies while
// the relay still applies the old ones. A capsule therefore never sees
// a moment in which a newly denied destination is reachable.
func Reload(controlDir string, r io.Reader) error {
	payload, err := io.ReadAll(io.LimitReader(r, MaxPolicyBytes))
	if err != nil {
		return err
	}
	if len(payload) == 0 {
		return errors.New("no policy supplied")
	}
	var incoming egress.Policy
	if err := json.Unmarshal(payload, &incoming); err != nil {
		return err
	}
	return installPolicy(controlDir, func(current egress.Policy) (egress.Policy, error) {
		next := egress.Policy{
			InternalSubnet: current.InternalSubnet,
			UplinkSubnet:   current.UplinkSubnet,
			Allow:          incoming.Allow,
			Deny:           incoming.Deny,
		}
		// Validated here, not in installPolicy: these prefixes arrived
		// from outside this process, and this is the trust boundary they
		// cross. DenyAll's do not, so hoisting the check into the shared
		// path would misread why it is here.
		return next, next.Validate()
	})
}

// DenyAll installs the most restrictive posture a live gateway can
// take without dying: a policy whose deny set covers the entire IPv4
// space, applied to both the kernel ruleset and the relay's own check.
//
// It exists for the case the controller cannot prove a policy is
// current — discovery failed, or a required deny could not be
// installed. Continuing to relay under a policy that predates a changed
// environment is the failure mode this replaces: the old set is not
// "narrower", it is merely older, and a subnet that appeared since is
// reachable under it.
func DenyAll(controlDir string) error {
	return installPolicy(controlDir, func(current egress.Policy) (egress.Policy, error) {
		return egress.Policy{
			InternalSubnet: current.InternalSubnet,
			UplinkSubnet:   current.UplinkSubnet,
			Deny:           []string{"0.0.0.0/0"},
		}, nil
	})
}

// installPolicy is the part both paths share: read what is in force,
// let the caller build the successor from it, then apply the successor
// to the kernel before writing the file the relay reads. That order is
// the whole point — the ruleset tightens first, so a newly denied
// destination is never reachable in between.
//
// The two legs always come from the policy in force. Which networks a
// gateway bridges is a fact of its own creation, not something a later
// discovery pass may redefine.
func installPolicy(controlDir string, build func(current egress.Policy) (egress.Policy, error)) error {
	path := PolicyPath(controlDir)
	inForce, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("no policy in force: %w", err)
	}
	var current egress.Policy
	if err := json.Unmarshal(inForce, &current); err != nil {
		return err
	}
	next, err := build(current)
	if err != nil {
		return err
	}
	legs, err := ClassifyLegs(next)
	if err != nil {
		return err
	}
	if err := ApplyFirewall(next, legs); err != nil {
		return err
	}
	encoded, err := json.Marshal(next)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, encoded)
}
