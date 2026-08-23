package docker

import "testing"

// The probe's df output is parsed positionally, which POSIX format
// guarantees; these fixtures are verbatim busybox output shapes.
func TestParseDFProbe(t *testing.T) {
	out := "Filesystem           1024-blocks    Used Available Capacity Mounted on\n" +
		"overlay              959786032 121456760 789495672  13% /\n" +
		"Filesystem              Inodes      Used Available Use% Mounted on\n" +
		"overlay               61054976   1610243  59444733   3% /\n"
	free, err := parseDFProbe(out)
	if err != nil {
		t.Fatal(err)
	}
	if free.FreeBytes != 789495672*1024 {
		t.Errorf("FreeBytes = %d; want %d", free.FreeBytes, int64(789495672)*1024)
	}
	if free.FreeInodes != 59444733 {
		t.Errorf("FreeInodes = %d; want 59444733", free.FreeInodes)
	}
}

// btrfs does not account inodes and reports zero totals; that must read
// as "unknown", never as an exhausted filesystem.
func TestParseDFProbeNoInodeAccounting(t *testing.T) {
	out := "Filesystem           1024-blocks    Used Available Capacity Mounted on\n" +
		"overlay               10485760   1048576   9437184  10% /\n" +
		"Filesystem              Inodes      Used Available Use% Mounted on\n" +
		"overlay                      0         0         0    0% /\n"
	free, err := parseDFProbe(out)
	if err != nil {
		t.Fatal(err)
	}
	if free.FreeInodes != -1 {
		t.Errorf("FreeInodes = %d; want -1 (unknown)", free.FreeInodes)
	}
}

func TestParseDFProbeRejectsGarbage(t *testing.T) {
	if _, err := parseDFProbe("no tables here\n"); err == nil {
		t.Fatal("garbage parsed as a df table")
	}
}

// TestParseDFProbeReportsAFullFilesystem is the measurement the pressure
// machine exists to catch, and the one the parser used to lose. Zero
// available blocks was indistinguishable from "no blocks table", so a full
// host produced an error — and a monitor pass treats a failed measurement as
// "keep the current level". Admission therefore stayed open on a host with
// nothing left, indefinitely, because every later probe failed identically.
func TestParseDFProbeReportsAFullFilesystem(t *testing.T) {
	out := "Filesystem           1024-blocks    Used Available Capacity Mounted on\n" +
		"overlay               10230000  10230000         0     100% /\n" +
		"Filesystem              Inodes      Used Available Use% Mounted on\n" +
		"overlay                 655360    655360         0  100% /\n"
	free, err := parseDFProbe(out)
	if err != nil {
		t.Fatalf("a full filesystem was reported as a parse failure: %v", err)
	}
	if free.FreeBytes != 0 {
		t.Errorf("FreeBytes = %d; want 0", free.FreeBytes)
	}
	if free.FreeInodes != 0 {
		t.Errorf("FreeInodes = %d; want 0", free.FreeInodes)
	}
}
