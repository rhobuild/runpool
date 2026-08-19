package doctor

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/rhobuild/runpool/internal/config"
	"github.com/rhobuild/runpool/internal/platform/docker"
)

// configuredSwap is the swap that will actually be requested on a container,
// so it sums tier envelopes only. host.reserve.swap is capacity withheld from
// scheduling and never reaches a container: counting it here would demand
// cgroup swap accounting for a limit Runpool never sets. The host-total gate
// in physicalCapacity does count the reserve, because that question is
// whether the host has the swap, not whether Docker can cap it.
func configuredSwap(cfg *config.Config) int64 {
	if cfg == nil {
		return 0
	}
	var total int64
	for _, tier := range cfg.Tiers {
		total += int64(tier.Resources.Swap)
	}
	return total
}

// checkCapacity reports scheduling policy and binding contention. More
// bindings than tier parallelism is legal because they share credits, but a
// silent binding may wait one discovery rotation before its first job is seen.
func checkCapacity(cfg *config.Config) Result {
	bindings := map[string]int{}
	for _, target := range cfg.Targets {
		for _, binding := range target.Tiers {
			bindings[binding.TierID]++
		}
	}
	total := 0
	totalBindings := 0
	var contended []string
	for _, tier := range cfg.Tiers {
		total += tier.Parallelism
		totalBindings += bindings[tier.ID]
		if n := bindings[tier.ID]; n > tier.Parallelism {
			contended = append(contended, fmt.Sprintf("%s (parallelism %d, %d bindings)", tier.ID, tier.Parallelism, n))
		}
	}
	if cfg.Scheduling.Parallelism != nil && totalBindings > *cfg.Scheduling.Parallelism {
		contended = append(contended, fmt.Sprintf("instance (parallelism %d, %d bindings)",
			*cfg.Scheduling.Parallelism, totalBindings))
	}
	detail := fmt.Sprintf("aggregate tier parallelism %d across %d tiers", total, len(cfg.Tiers))
	if cfg.Scheduling.Parallelism != nil {
		detail += fmt.Sprintf("; instance parallelism %d", *cfg.Scheduling.Parallelism)
	} else {
		detail += "; tiers are independent"
	}
	if len(contended) > 0 {
		return Result{"capacity", Warn, detail + "; contended: " + strings.Join(contended, ", "),
			"bindings share tier capacity and rotate discovery; raise tier parallelism if first-job latency matters"}
	}
	return Result{"capacity", Pass, detail, ""}
}

// checkPhysicalCapacity fails before admission when the worst workload set
// plus the configured host reserve cannot fit on the daemon host.
func checkPhysicalCapacity(ctx context.Context, cfg *config.Config, d daemonInfo) Result {
	if d == nil {
		return Result{"physical capacity", Fail, "daemon not connected", ""}
	}
	info, err := d.Info(ctx)
	if err != nil {
		return Result{"physical capacity", Fail, err.Error(), ""}
	}
	return physicalCapacity(cfg, info)
}

func physicalCapacity(cfg *config.Config, info docker.HostInfo) Result {
	required := capacityRequirement(cfg)
	needMem := saturatingAdd(required.memory, int64(cfg.Host.Reserve.Memory))
	needCPU := saturatingAdd(required.nanoCPUs, int64(cfg.Host.Reserve.CPU))
	needSwap := saturatingAdd(required.swap, int64(cfg.Host.Reserve.Swap))
	hostCPU := int64(info.NCPU) * 1_000_000_000
	detail := fmt.Sprintf("admitted workloads need %s memory, %s swap and %.1f CPU; reserve adds %s memory, %s swap and %.1f CPU; host has %s memory, %s swap and %d CPU",
		config.ByteSize(required.memory), config.ByteSize(required.swap), float64(required.nanoCPUs)/1e9,
		cfg.Host.Reserve.Memory, cfg.Host.Reserve.Swap, float64(cfg.Host.Reserve.CPU)/1e9,
		config.ByteSize(info.MemTotalBytes), config.ByteSize(info.SwapTotalBytes), info.NCPU)
	if needMem > info.MemTotalBytes {
		return Result{"physical capacity", Fail, detail,
			"the worst admitted workload set plus the host reserve exceeds host memory; lower parallelism, tier memory or the reserve"}
	}
	if needCPU > hostCPU {
		return Result{"physical capacity", Fail, detail,
			"the worst admitted workload set plus the host reserve exceeds host CPU; lower parallelism, tier cpu or the reserve"}
	}
	if needSwap > 0 && !info.SwapTotalKnown {
		return Result{"physical capacity", Fail, detail,
			"host swap capacity could not be read; use the local daemon and expose /proc/meminfo to the controller, or set every tier swap and host.reserve.swap to 0B"}
	}
	if needSwap > info.SwapTotalBytes {
		return Result{"physical capacity", Fail, detail,
			"the worst admitted workload set plus the host reserve exceeds host swap; provision encrypted swap or lower configured swap"}
	}
	return Result{"physical capacity", Pass, detail, ""}
}

type capacityNeeds struct {
	memory   int64
	swap     int64
	nanoCPUs int64
}

// capacityRequirement calculates each resource dimension independently. With
// a global limit, the most expensive N tier envelopes are the conservative
// workload set for that dimension; without one, every tier may fill at once.
func capacityRequirement(cfg *config.Config) capacityNeeds {
	totalParallelism := 0
	for _, tier := range cfg.Tiers {
		totalParallelism += tier.Parallelism
	}
	limit := totalParallelism
	if cfg.Scheduling.Parallelism != nil && *cfg.Scheduling.Parallelism < limit {
		limit = *cfg.Scheduling.Parallelism
	}
	return capacityNeeds{
		memory:   sumLargest(cfg.Tiers, limit, func(r config.Resources) int64 { return int64(r.Memory) }),
		swap:     sumLargest(cfg.Tiers, limit, func(r config.Resources) int64 { return int64(r.Swap) }),
		nanoCPUs: sumLargest(cfg.Tiers, limit, func(r config.Resources) int64 { return int64(r.CPU) }),
	}
}

type weightedResource struct {
	value int64
	count int
}

func sumLargest(tiers []config.Tier, limit int, value func(config.Resources) int64) int64 {
	values := make([]weightedResource, len(tiers))
	for i, tier := range tiers {
		values[i] = weightedResource{value: value(tier.Resources), count: tier.Parallelism}
	}
	sort.Slice(values, func(i, j int) bool { return values[i].value > values[j].value })
	var total int64
	for _, candidate := range values {
		count := min(candidate.count, limit)
		if count <= 0 {
			break
		}
		if candidate.value > 0 && int64(count) > (math.MaxInt64-total)/candidate.value {
			return math.MaxInt64
		}
		total += candidate.value * int64(count)
		limit -= count
	}
	return total
}

func saturatingAdd(a, b int64) int64 {
	if b > math.MaxInt64-a {
		return math.MaxInt64
	}
	return a + b
}
