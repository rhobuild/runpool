// Package disk is the disk-pressure state machine. It is pure: the
// caller measures (daemon filesystem free space and inodes, managed
// cache bytes) and the machine says what level the host is at and what
// that level obliges — GC toward the low watermark, closing admission,
// or failing closed. Keeping it pure is what makes every transition,
// including the hysteresis on recovery, enumerable in a hermetic test.
//
// The levels, worst first:
//
//	hard_emergency  free space below the hard floor: fail closed. New
//	                work is refused, nothing that holds state is
//	                deleted, the operator is alerted.
//	soft_emergency  free space below the soft floor or the configured
//	                host reserve: admission closes and GC turns
//	                aggressive on free resources.
//	high            managed cache bytes crossed the high watermark:
//	                GC runs until the low watermark.
//	normal          none of the above.
//
// Recovery is hysteretic so the actions do not flap at a boundary: an
// emergency exits only after free space clears the floor by the same
// band that separates the soft and hard floors, and high exits only at
// the low watermark — the configured pair is itself the hysteresis.
package disk

import "fmt"

type Level int

const (
	Normal Level = iota
	High
	SoftEmergency
	HardEmergency
)

func (l Level) String() string {
	switch l {
	case Normal:
		return "normal"
	case High:
		return "high"
	case SoftEmergency:
		return "soft_emergency"
	case HardEmergency:
		return "hard_emergency"
	}
	return fmt.Sprintf("level(%d)", int(l))
}

// ParseLevel is the inverse of String, for the persisted level.
func ParseLevel(s string) (Level, error) {
	for _, l := range []Level{Normal, High, SoftEmergency, HardEmergency} {
		if l.String() == s {
			return l, nil
		}
	}
	return Normal, fmt.Errorf("unknown pressure level %q", s)
}

// Inode floors. Filesystems that account inodes run out of them the
// same way they run out of bytes — a node_modules tree is millions of
// tiny files — but no configuration knob exists for a quantity
// operators never think in, so the floors are fixed where any real CI
// host is either fine or in genuine trouble.
const (
	softInodeFloor = 100_000
	hardInodeFloor = 10_000
	// inodeRecoveryBand is the inode dimension's hysteresis margin, in the
	// same proportion the byte floors use: an emergency entered on inodes
	// is left only once free inodes clear the floor by this much. Without
	// it a workload hovering at the floor flips admission on every pass.
	inodeRecoveryBand = softInodeFloor - hardInodeFloor
)

// Thresholds are the configured boundaries, all in bytes of the
// daemon's storage filesystem except the watermark pair, which is a
// percentage of the managed cache budget.
type Thresholds struct {
	MaxManagedBytes int64
	HighPct, LowPct int
	SoftFreeBytes   int64
	HardFreeBytes   int64
	// ReserveFreeBytes is host.reserve.freeDisk: free space the
	// operator told scheduling to keep its hands off. Falling under it
	// means the reserve cannot be maintained, which closes admission
	// exactly like the soft floor.
	ReserveFreeBytes int64
}

// Facts are one measurement pass.
type Facts struct {
	FreeBytes int64
	// FreeInodes is -1 when the filesystem does not account inodes.
	FreeInodes int64
	// ManagedBytes is the daemon-measured total of this instance's
	// cache-lane volumes.
	ManagedBytes int64
}

// softFloor is the effective admission floor: the soft emergency
// threshold or the host reserve, whichever asks for more.
func (t Thresholds) softFloor() int64 {
	if t.ReserveFreeBytes > t.SoftFreeBytes {
		return t.ReserveFreeBytes
	}
	return t.SoftFreeBytes
}

// Next computes the level after one measurement, given the previous
// level. Worst condition wins; recovery passes through the hysteresis
// band before an emergency releases its actions.
func Next(prev Level, f Facts, t Thresholds) Level {
	soft := t.softFloor()
	// The band is the configured soft-hard separation, not a quantity
	// derived from the floor in force. Deriving it from softFloor made the
	// exit edge `2*softFloor - hard`, which grows with the host reserve:
	// with a 20GiB reserve and a 10GiB hard floor, an emergency entered
	// below 20GiB free could only be left above 30GiB. A data root whose
	// healthy steady state sits between those two latches closed forever,
	// and the disk monitor reloads that across restarts.
	band := t.SoftFreeBytes - t.HardFreeBytes
	if band < 0 {
		band = 0
	}

	inodesKnown := f.FreeInodes >= 0
	switch {
	case f.FreeBytes < t.HardFreeBytes || (inodesKnown && f.FreeInodes < hardInodeFloor):
		return HardEmergency
	case f.FreeBytes < soft || (inodesKnown && f.FreeInodes < softInodeFloor):
		return SoftEmergency
	// Recovery clears the entry edge by one band on whichever dimension
	// triggered. Testing bytes alone let an inode emergency — which by
	// construction happens while bytes are plentiful — exit the instant
	// free inodes ticked one over the floor, flipping admission and
	// wiping the free lane pool on every pass.
	case prev >= SoftEmergency && f.FreeBytes < soft+band:
		return SoftEmergency
	case prev >= SoftEmergency && inodesKnown && f.FreeInodes < softInodeFloor+inodeRecoveryBand:
		return SoftEmergency
	}

	if t.MaxManagedBytes > 0 {
		high := t.MaxManagedBytes * int64(t.HighPct) / 100
		low := t.MaxManagedBytes * int64(t.LowPct) / 100
		switch {
		case f.ManagedBytes >= high:
			return High
		case prev >= High && f.ManagedBytes > low:
			return High
		}
	}
	return Normal
}

// AdmissionClosed says whether new work may start at this level. The
// distinction from GC posture is deliberate: high pressure garbage
// collects but keeps admitting; the emergencies do not.
func (l Level) AdmissionClosed() bool { return l >= SoftEmergency }

// WantsGC says whether this level obliges a collection pass, and
// Aggressive whether that pass may take free lanes before their TTL.
func (l Level) WantsGC() bool    { return l == High || l == SoftEmergency }
func (l Level) Aggressive() bool { return l == SoftEmergency }
