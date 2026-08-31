package store

import (
	"errors"
	"strconv"
	"time"
)

// The egress sandbox's last completed rediscovery pass, as two
// instance-scoped singletons.
//
// A table of their own would match this store's own precedents more
// closely -- pressure and provider_binding_contact are both a
// background loop's last outcome, and provider_binding_contact is this
// exact shape, a timestamp and an error string. They are in meta anyway,
// and the reason is not that meta is the better home: it is that a new
// table is a migration, the baseline is immutable, and the first
// migration this project writes will meet deployed v1.0.0 databases.
// Spending that on one timestamp and one reason is the wrong trade. If
// this ever needs a third field, it should take the table instead.
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
	At time.Time
	// Error is why it failed, and empty when it did not. A non-empty
	// value means every gateway on this host was closed to all egress.
	Error string
}

// SetSandboxPass records the outcome of one rediscovery pass.
func (t *Tx) SetSandboxPass(p SandboxPass) error {
	if p.At.IsZero() {
		return errors.New("sandbox pass time must not be zero")
	}
	if err := t.metaSet(sandboxPassAtKey, strconv.FormatInt(p.At.Unix(), 10)); err != nil {
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
	return &SandboxPass{At: unixTime(seconds), Error: reason}, nil
}
