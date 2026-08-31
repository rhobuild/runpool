package allocator

import (
	"strconv"
	"testing"

	"github.com/rhobuild/runpool/internal/assignment"
)

const (
	supportedBindingCount = 10_000
	supportedParallelism  = 10_000
)

// The release budget at the supported ceiling is 20 ms for a forced rebuild,
// at most 96 allocations for one tier or 768 for 100 tiers, and zero
// allocations for a cached read. Wall time is reported rather than asserted on
// heterogeneous machines; TestAllocationPlanWorkIsBounded gates the
// hardware-independent work and allocation budgets with the same fixtures.
func BenchmarkAllocationPlanAtSupportedScale(b *testing.B) {
	for _, profile := range []struct {
		name   string
		global bool
	}{
		{name: "independent-tier"},
		{name: "global-limit", global: true},
	} {
		b.Run(profile.name, func(b *testing.B) {
			allocator, keys := supportedScaleAllocator(b, profile.global)
			allocator.Plan()
			b.Run("rebuild", func(b *testing.B) {
				b.ReportAllocs()
				for iteration := range b.N {
					allocator.SetAssignedDemand(keys[0], supportedParallelism+iteration%2)
					if allocator.Plan().DesiredCapacity(keys[0]) == 0 {
						b.Fatal("rebuilt plan lost demand")
					}
				}
			})
			b.Run("cached-read", func(b *testing.B) {
				plan := allocator.Plan()
				b.ReportAllocs()
				for range b.N {
					if allocator.Plan() != plan {
						b.Fatal("cached read rebuilt the plan")
					}
				}
			})
		})
	}
}

func TestAllocationPlanWorkIsBounded(t *testing.T) {
	for _, profile := range []struct {
		name               string
		global             bool
		maximumAllocations float64
	}{
		{name: "independent tier", maximumAllocations: 96},
		{name: "global limit", global: true, maximumAllocations: 768},
	} {
		t.Run(profile.name, func(t *testing.T) {
			allocator, keys := supportedScaleAllocator(t, profile.global)
			plan := allocator.Plan()
			const maximumVisits = supportedBindingCount * 32
			if plan.allocationVisits > maximumVisits {
				t.Fatalf("plan inspected %d candidates; budget is %d at %d bindings and %d credits",
					plan.allocationVisits, maximumVisits, supportedBindingCount, supportedParallelism)
			}

			toggle := 0
			allocations := testing.AllocsPerRun(3, func() {
				toggle ^= 1
				allocator.SetAssignedDemand(keys[0], supportedParallelism+toggle)
				allocator.Plan()
			})
			if allocations > profile.maximumAllocations {
				t.Fatalf("plan rebuild allocated %.0f objects; budget is %.0f at %d bindings",
					allocations, profile.maximumAllocations, supportedBindingCount)
			}
		})
	}
}

func supportedScaleAllocator(tb testing.TB, global bool) (*Allocator, []assignment.BindingKey) {
	tb.Helper()
	allocator := New()
	tierCount := 1
	bindingsPerTier := supportedBindingCount
	if global {
		allocator = NewWithGlobalParallelism(supportedParallelism)
		tierCount = 100
		bindingsPerTier = supportedBindingCount / tierCount
	}
	keys := make([]assignment.BindingKey, supportedBindingCount)
	index := 0
	for tier := range tierCount {
		tierID := assignment.TierID("tier-" + strconv.Itoa(tier))
		tierParallelism := supportedParallelism
		if global {
			tierParallelism = bindingsPerTier
		}
		for range bindingsPerTier {
			key := assignment.BindingKey("binding-" + strconv.Itoa(index))
			keys[index] = key
			if err := allocator.Register(tierID, key, tierParallelism); err != nil {
				tb.Fatal(err)
			}
			allocator.SessionOpened(key)
			allocator.SetAssignedDemand(key, supportedParallelism)
			index++
		}
	}
	return allocator, keys
}
