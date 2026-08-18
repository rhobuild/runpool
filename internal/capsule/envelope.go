package capsule

import (
	"strings"

	"github.com/rhobuild/runpool/internal/config"
)

// The gateway's share of a lease's tier envelope. Configuration validation
// refuses a tier too small to reserve these resources.
const (
	GatewayReserveMemory = config.GatewayReserveMemory
	GatewayReserveCPUs   = config.GatewayReserveCPUs
	GatewayReservePIDs   = config.GatewayReservePIDs
)

// Envelope is a resolved resource budget in the units Docker takes.
type Envelope struct {
	MemoryBytes     int64
	MemorySwapBytes int64
	NanoCPUs        int64
	PIDsLimit       int64
}

// SplitEnvelope divides a tier between the capsule and its gateway. Without a
// gateway the capsule takes the whole tier. With one, the two shares add up to
// exactly the configured tier.
func SplitEnvelope(r config.Resources, withGateway bool) Envelope {
	e := Envelope{
		MemoryBytes:     int64(r.Memory),
		MemorySwapBytes: int64(r.Memory) + int64(r.Swap),
		NanoCPUs:        int64(r.CPU),
		PIDsLimit:       r.PIDs,
	}
	if !withGateway {
		return e
	}
	e.MemoryBytes -= GatewayReserveMemory
	// Swap is additional capacity. The gateway's RAM reserve therefore
	// reduces Docker's total by the same amount and leaves swap with the
	// workload capsule.
	e.MemorySwapBytes -= GatewayReserveMemory
	e.NanoCPUs -= GatewayReserveCPUs
	e.PIDsLimit -= GatewayReservePIDs
	return e
}

// GatewayEnvelope returns the gateway's fixed share of a tier.
func GatewayEnvelope() Envelope {
	return Envelope{
		MemoryBytes:     GatewayReserveMemory,
		MemorySwapBytes: GatewayReserveMemory,
		NanoCPUs:        GatewayReserveCPUs,
		PIDsLimit:       GatewayReservePIDs,
	}
}

// LeaseCgroupParent names the parent cgroup shared by a lease's containers.
// systemd expects a slice unit while cgroupfs expects a path. An unknown driver
// returns no parent so callers cannot guess a daemon-specific representation.
func LeaseCgroupParent(driver, leaseID string) string {
	name := sliceSafe(leaseID)
	if name == "" {
		return ""
	}
	switch driver {
	case "systemd":
		return "runpool-lease-" + name + ".slice"
	case "cgroupfs":
		return "/runpool-lease-" + name
	default:
		return ""
	}
}

// KnownCgroupDriver reports whether a parent cgroup can be written for
// this daemon's driver.
//
// It is exported because the answer has to be reached before a capsule
// exists. LeaseCgroupParent returns an empty parent for a driver it
// cannot address, and Docker reads an empty CgroupParent as "no parent" —
// so the capsule and its gateway would land in separate budgets and the
// tier would quietly stop being their sum. Nothing later in the launch
// says anything about it.
func KnownCgroupDriver(driver string) bool {
	return driver == "systemd" || driver == "cgroupfs"
}

// sliceSafe reduces a lease id to the bounded alphanumeric component accepted
// by systemd unit names. Lease ids are currently hexadecimal, but the cgroup
// contract does not depend on that implementation detail.
func sliceSafe(leaseID string) string {
	var b strings.Builder
	for _, r := range leaseID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		}
		if b.Len() == 16 {
			break
		}
	}
	return b.String()
}
