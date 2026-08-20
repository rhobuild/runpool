package gateway

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/rhobuild/runpool/internal/egress"
	"github.com/rhobuild/runpool/internal/platform/atomicfile"
)

// MaxPolicyBytes bounds a policy document. The real one is a few
// kilobytes of CIDRs; anything larger is a mistake or an attempt.
const MaxPolicyBytes = 1 << 20

// policyLockFile serializes the installers against each other. It lives
// beside the policy on the same tmpfs, so it is created with the control
// surface and dies with the container.
const policyLockFile = "policy.lock"

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
	stamp   [sha256.Size]byte
	// generation counts installed policies. A pooled HTTP connection was
	// authorised by whichever policy was in force when it was dialled, and
	// http.Transport reuses it without consulting the dialer again, so the
	// relay needs to know the policy moved in order to drop those
	// connections. A counter says that; comparing deciders does not.
	generation uint64
}

// Current returns the compiled policy in force, recompiling it when the
// document's bytes have changed since the last read.
//
// What counts as a change is the hash of the contents, not the pair of
// modification time and size. Linux stamps mtime from the coarse clock,
// which advances once per tick, so two documents of equal length
// installed inside one tick carry the same pair — and the second install
// would then never reach the relay at all: the decider would stay as it
// was, the generation would not move, and the pooled transport would not
// be retired either. A tightening would silently not take effect. The
// document is a few kilobytes on tmpfs, so reading it every time costs
// less than the case it rules out.
//
// The read is inside the lock, with the decision it feeds. Outside it,
// two readers crossing an install can read different documents and
// finish in the other order, so the one that read first would store what
// the other read under the stamp of what it did not.
func (s *PolicyStore) Current() (*egress.Decider, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	raw, err := readPolicy(s.Path)
	if err != nil {
		return nil, err
	}
	stamp := sha256.Sum256(raw)
	if s.decider != nil && s.stamp == stamp {
		return s.decider, nil
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

// readPolicy reads a policy document, refusing to hold more of one than
// a policy may be. The bound is the same MaxPolicyBytes ParsePolicy
// applies, taken before the bytes are resident rather than after: this
// read now happens on every call rather than once per change, and the
// gateway it happens in has 128 MiB.
func readPolicy(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, MaxPolicyBytes+1))
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
		// Validated here because these prefixes arrived from outside
		// this process, and this is the boundary they cross. What
		// counts as valid — including that no allow may be broader than
		// a range the baseline withholds — belongs to the policy, so
		// this channel and the configuration file are held to one rule.
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
	return installPolicyWith(controlDir, build, applyToKernel)
}

// applyToKernel identifies this container's two legs and installs the
// ruleset for the policy about to take effect.
//
// It is a parameter of the install rather than a step inside it because
// what the install owns — the lock, the read-modify-write, the order of
// kernel before file — is only observable on a host that has both legs
// and NET_ADMIN. Named here, those properties can be exercised
// anywhere, and the step that genuinely needs a gateway container is
// the one the live contract covers.
func applyToKernel(next egress.Policy) error {
	legs, err := ClassifyLegs(next)
	if err != nil {
		return err
	}
	return ApplyFirewall(next, legs)
}

func installPolicyWith(controlDir string,
	build func(current egress.Policy) (egress.Policy, error),
	apply func(next egress.Policy) error) error {

	// The two callers reach this from separate `docker exec` processes
	// into the same container, so the lock has to be one the kernel
	// holds. What it buys is order: each installer reads the policy
	// actually in force and builds its successor from that, instead of
	// from a document another installer is midway through replacing.
	//
	// It orders them; it does not rank them. Every installer builds from
	// what it reads and the last to run is what stays, so a reload that
	// starts after an emergency close is what the relay ends up with.
	// Nothing here prevents that, and nothing needs to: the emergency
	// close is only reached from closeGateway, which removes the
	// container whatever the install returned, so the policy left behind
	// outlives nothing.
	//
	// It blocks rather than trying, so an installer waits its turn
	// instead of failing because another held the lock.
	lock, err := os.OpenFile(filepath.Join(controlDir, policyLockFile), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("policy lock: %w", err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("policy lock: %w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

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
	if err := apply(next); err != nil {
		return err
	}
	encoded, err := json.Marshal(next)
	if err != nil {
		return err
	}
	return atomicfile.Replace(path, encoded, 0o600, -1, -1)
}
