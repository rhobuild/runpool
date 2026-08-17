package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// ErrLockHeld reports that another controller process owns the state
// directory; the caller should enter standby instead of proceeding.
var ErrLockHeld = errors.New("state lock is held by another process")

// LockFile is the singleton lock's name inside the state directory.
const LockFile = "runpool.lock"

// Lock is the singleton controller lock: an flock on a file in the state
// volume. The kernel releases it when the owning process dies, which is
// exactly the handover semantics a crashed controller needs — no lease
// record to expire, no heartbeat to tend.
type Lock struct {
	f *os.File
}

// TryAcquire takes the lock without blocking. A second controller (a
// PaaS start-before-stop overlap, an operator command) gets ErrLockHeld
// and must not advertise capacity or create resources.
func TryAcquire(dir string) (*Lock, error) {
	f, err := os.OpenFile(filepath.Join(dir, LockFile), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLockHeld
		}
		return nil, fmt.Errorf("state lock: %w", err)
	}
	// The pid is diagnostic only; the flock is the authority.
	f.Truncate(0)
	f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	return &Lock{f: f}, nil
}

// Release drops the lock; the file itself remains for the next owner.
func (l *Lock) Release() error {
	if err := syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN); err != nil {
		return err
	}
	return l.f.Close()
}
