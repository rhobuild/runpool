package disk

import "testing"

// GiB-scale fixture: budget 10 GiB with watermarks at 80/60, soft floor
// 5 GiB, hard floor 1 GiB, no extra reserve. The soft-hard band (4 GiB)
// is the recovery hysteresis.
var th = Thresholds{
	MaxManagedBytes: 10 << 30,
	HighPct:         80,
	LowPct:          60,
	SoftFreeBytes:   5 << 30,
	HardFreeBytes:   1 << 30,
}

const gib = int64(1) << 30

func facts(freeGiB, managedGiB int64) Facts {
	return Facts{FreeBytes: freeGiB * gib, FreeInodes: -1, ManagedBytes: managedGiB * gib}
}

// TestTransitions enumerates the machine: every row is (previous level,
// measurement) -> level. The table is the reproducible record the gate
// asks for.
func TestTransitions(t *testing.T) {
	cases := []struct {
		name string
		prev Level
		f    Facts
		want Level
	}{
		{"plenty of everything", Normal, facts(100, 2), Normal},
		{"managed crosses high watermark", Normal, facts(100, 8), High},
		{"exactly at high watermark", Normal, facts(100, 8), High},
		{"high holds above low watermark", High, facts(100, 7), High},
		{"high exits at low watermark", High, facts(100, 6), Normal},
		{"normal stays normal between watermarks", Normal, facts(100, 7), Normal},

		{"free under soft floor", Normal, facts(4, 2), SoftEmergency},
		{"free under hard floor", Normal, facts(0, 2), HardEmergency},
		{"hard beats high", Normal, facts(0, 9), HardEmergency},
		{"soft beats high", Normal, facts(4, 9), SoftEmergency},

		// Recovery hysteresis: the band is soft-hard = 4 GiB, so an
		// emergency releases only at free >= 5+4 = 9 GiB.
		{"soft holds inside the band", SoftEmergency, facts(6, 2), SoftEmergency},
		{"soft holds at band edge", SoftEmergency, facts(8, 2), SoftEmergency},
		{"soft releases past the band", SoftEmergency, facts(9, 2), Normal},
		{"hard falls back to soft first", HardEmergency, facts(2, 2), SoftEmergency},
		{"hard releases only past the band", HardEmergency, facts(9, 2), Normal},
		{"release lands on high when managed is high", SoftEmergency, facts(9, 9), High},
	}
	for _, tc := range cases {
		if got := Next(tc.prev, tc.f, th); got != tc.want {
			t.Errorf("%s: Next(%s, free=%d managed=%d) = %s; want %s",
				tc.name, tc.prev, tc.f.FreeBytes/gib, tc.f.ManagedBytes/gib, got, tc.want)
		}
	}
}

// TestReserveRaisesTheFloor: host.reserve.freeDisk is a promise to the
// host; scheduling must close before it would be broken.
func TestReserveRaisesTheFloor(t *testing.T) {
	withReserve := th
	withReserve.ReserveFreeBytes = 20 * gib

	if got := Next(Normal, facts(10, 2), withReserve); got != SoftEmergency {
		t.Errorf("free below reserve = %s; want soft_emergency", got)
	}
	// The reserve raises the floor; it does not widen the hysteresis. The
	// band stays the configured soft-hard separation (5-1 = 4 GiB), so the
	// exit edge is 20+4 = 24.
	//
	// Deriving the band from the floor instead made the edge
	// `2*floor - hard`, which grows without bound as the reserve grows: at
	// these thresholds a 20 GiB reserve demanded 39 GiB free to recover. A
	// data root whose healthy steady state sits between entry and exit
	// latches closed forever, and the disk monitor reloads that across
	// restarts, so a restart does not clear it either.
	if got := Next(SoftEmergency, facts(23, 2), withReserve); got != SoftEmergency {
		t.Errorf("inside the recovery band = %s; want soft_emergency held", got)
	}
	if got := Next(SoftEmergency, facts(24, 2), withReserve); got != Normal {
		t.Errorf("past the recovery band = %s; want normal", got)
	}
	// A reserve far above the soft floor must not make recovery harder in
	// proportion: the band is the same 4 GiB whatever the reserve is.
	huge := th
	huge.ReserveFreeBytes = 100 * gib
	if got := Next(SoftEmergency, facts(104, 2), huge); got != Normal {
		t.Errorf("a large reserve widened the recovery band: %s", got)
	}
}

// TestInodeEmergencyHasHysteresis: an emergency entered on inodes happens
// while bytes are plentiful, so testing bytes alone let it exit the instant
// free inodes ticked one over the floor. A workload hovering at the floor
// then flipped admission on every pass — withdrawing announced capacity and
// wiping the free lane pool each time.
func TestInodeEmergencyHasHysteresis(t *testing.T) {
	plentiful := func(inodes int64) Facts {
		return Facts{FreeBytes: 500 * gib, FreeInodes: inodes, ManagedBytes: 0}
	}
	if got := Next(Normal, plentiful(softInodeFloor-1), th); got != SoftEmergency {
		t.Fatalf("below the inode floor = %s; want soft_emergency", got)
	}
	if got := Next(SoftEmergency, plentiful(softInodeFloor+1), th); got != SoftEmergency {
		t.Errorf("one inode over the floor = %s; want the emergency held", got)
	}
	if got := Next(SoftEmergency, plentiful(softInodeFloor+inodeRecoveryBand), th); got != Normal {
		t.Errorf("past the inode recovery band = %s; want normal", got)
	}
	// A filesystem that does not account inodes must not be held by this.
	if got := Next(SoftEmergency, facts(500, 0), th); got != Normal {
		t.Errorf("unaccounted inodes held an emergency: %s", got)
	}
}

// TestInodeFloors: running out of inodes is running out of disk, but a
// filesystem that does not account them must never look empty.
func TestInodeFloors(t *testing.T) {
	f := facts(100, 2)
	f.FreeInodes = 5_000
	if got := Next(Normal, f, th); got != HardEmergency {
		t.Errorf("5k inodes = %s; want hard_emergency", got)
	}
	f.FreeInodes = 50_000
	if got := Next(Normal, f, th); got != SoftEmergency {
		t.Errorf("50k inodes = %s; want soft_emergency", got)
	}
	f.FreeInodes = -1
	if got := Next(Normal, f, th); got != Normal {
		t.Errorf("unknown inodes = %s; want normal", got)
	}
}

// TestObligations pins what each level means for the rest of the
// system; renumbering the constants must not silently invert a gate.
func TestObligations(t *testing.T) {
	for _, tc := range []struct {
		l          Level
		closed, gc bool
		aggressive bool
	}{
		{Normal, false, false, false},
		{High, false, true, false},
		{SoftEmergency, true, true, true},
		{HardEmergency, true, false, false},
	} {
		if tc.l.AdmissionClosed() != tc.closed || tc.l.WantsGC() != tc.gc || tc.l.Aggressive() != tc.aggressive {
			t.Errorf("%s: closed=%v gc=%v aggressive=%v; want %v %v %v", tc.l,
				tc.l.AdmissionClosed(), tc.l.WantsGC(), tc.l.Aggressive(), tc.closed, tc.gc, tc.aggressive)
		}
	}
}

func TestLevelStringsRoundTrip(t *testing.T) {
	// The literals, not String() fed back to ParseLevel: ParseLevel is
	// defined as the inverse of String, so a round trip can catch a
	// collision and never a rename -- and these four strings are a
	// durable contract, persisted in the pressure row and published in
	// the runbook. A controller upgraded across a rename fails ParseLevel
	// in resume, which returns before the admission gate is restored, and
	// admits into the emergency it was resuming.
	for want, l := range map[string]Level{
		"normal":         Normal,
		"high":           High,
		"soft_emergency": SoftEmergency,
		"hard_emergency": HardEmergency,
	} {
		if got := l.String(); got != want {
			t.Errorf("%d renders %q; %q is what a persisted row and the runbook hold", int(l), got, want)
		}
		got, err := ParseLevel(want)
		if err != nil || got != l {
			t.Errorf("ParseLevel(%q) = %v, %v; a stored level must parse back", want, got, err)
		}
	}
	if _, err := ParseLevel("frobnicated"); err == nil {
		t.Error("unknown level parsed")
	}
}
