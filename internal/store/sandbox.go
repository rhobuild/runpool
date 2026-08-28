package store

import "strconv"

// The egress sandbox's last completed rediscovery pass, as two
// instance-scoped singletons. They live in meta rather than a table of
// their own because that is what meta is for and because a new table is
// a migration, which this does not need: one timestamp and one reason
// are the whole of what a reader has to know.
const (
	sandboxPassAtKey    = "sandbox_pass_at"
	sandboxPassErrorKey = "sandbox_pass_error"
)

// SandboxPass is the outcome of the sandbox's last rediscovery.
//
// It is durable for the reason the disk verdict is: the pass runs inside
// the controller and the operator reads from another process, so an
// outcome kept in memory is an outcome nobody can be told about. And the
// failure it records is not a degraded one -- a rediscovery that cannot
// be trusted closes every gateway to all egress, which is correct and
// is also every running job losing its network at once.
type SandboxPass struct {
	// At is when the pass completed, successful or not.
	At int64
	// Error is why it failed, and empty when it did not. A non-empty
	// value means every gateway on this host was closed to all egress.
	Error string
}

// SetSandboxPass records the outcome of one rediscovery pass.
func (t *Tx) SetSandboxPass(p SandboxPass) error {
	if err := t.metaSet(sandboxPassAtKey, strconv.FormatInt(p.At, 10)); err != nil {
		return err
	}
	return t.metaSet(sandboxPassErrorKey, p.Error)
}

// SandboxPass returns the last recorded outcome, or nil when no pass has
// ever completed -- which a reader must treat as unknown rather than as
// healthy, the same way an absent disk verdict is not "normal".
func (t *Tx) SandboxPass() (*SandboxPass, error) {
	at, err := t.metaGet(sandboxPassAtKey)
	if err != nil || at == "" {
		return nil, err
	}
	seconds, err := strconv.ParseInt(at, 10, 64)
	if err != nil {
		return nil, err
	}
	reason, err := t.metaGet(sandboxPassErrorKey)
	if err != nil {
		return nil, err
	}
	return &SandboxPass{At: seconds, Error: reason}, nil
}
