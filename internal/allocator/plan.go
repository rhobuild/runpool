package allocator

import "github.com/rhobuild/runpool/internal/assignment"

// AllocationPlan is one immutable desired-capacity snapshot. Its generation
// changes only when inputs to allocation change; session cursors and remote
// acknowledgements do not rebuild an otherwise identical plan.
//
// The maps remain private and are never mutated after publication. A caller can
// retain a plan while later generations are built without observing partial
// updates.
type AllocationPlan struct {
	generation       uint64
	desired          map[assignment.BindingKey]int
	byTier           map[assignment.TierID]map[assignment.BindingKey]int
	discoveryByTier  map[assignment.TierID]assignment.BindingKey
	totalAdvertised  int
	allocationVisits int
}

// Generation identifies the allocator state that produced the plan.
func (p *AllocationPlan) Generation() uint64 {
	if p == nil {
		return 0
	}
	return p.generation
}

// DesiredCapacity returns the provider capacity assigned to a binding before
// the remote-capacity safety clamp is applied to its next poll.
func (p *AllocationPlan) DesiredCapacity(key assignment.BindingKey) int {
	if p == nil {
		return 0
	}
	return p.desired[key]
}

// DiscoveryHolder returns the binding holding a tier's discovery credit. With
// an instance-wide limit, only the tier containing the global holder has one.
func (p *AllocationPlan) DiscoveryHolder(tierID assignment.TierID) assignment.BindingKey {
	if p == nil {
		return ""
	}
	return p.discoveryByTier[tierID]
}

// DesiredCapacityByTier returns a copy so the published plan remains
// immutable to its callers.
func (p *AllocationPlan) DesiredCapacityByTier(tierID assignment.TierID) map[assignment.BindingKey]int {
	if p == nil || p.byTier[tierID] == nil {
		return nil
	}
	out := make(map[assignment.BindingKey]int, len(p.byTier[tierID]))
	for key, capacity := range p.byTier[tierID] {
		out[key] = capacity
	}
	return out
}

func (a *Allocator) invalidatePlan() {
	a.planGeneration++
	a.plan.Store(nil)
}

func (a *Allocator) planLocked() *AllocationPlan {
	if plan := a.plan.Load(); plan != nil {
		return plan
	}
	plan := a.buildPlan()
	a.plan.Store(plan)
	return plan
}

func (a *Allocator) buildPlan() *AllocationPlan {
	plan := &AllocationPlan{
		generation:      a.planGeneration,
		desired:         make(map[assignment.BindingKey]int, len(a.order)),
		byTier:          make(map[assignment.TierID]map[assignment.BindingKey]int, len(a.pools)),
		discoveryByTier: make(map[assignment.TierID]assignment.BindingKey, len(a.pools)),
	}
	for tierID, pool := range a.pools {
		tier := make(map[assignment.BindingKey]int, len(pool.order))
		plan.byTier[tierID] = tier
		for _, key := range pool.order {
			active := pool.state[key].active
			plan.desired[key] = active
			tier[key] = active
			plan.totalAdvertised += active
		}
		plan.allocationVisits += len(pool.order)
	}

	if a.globalParallelism > 0 {
		a.allocateGlobal(plan)
		a.setGlobalDiscovery(plan)
		return plan
	}
	for tierID, pool := range a.pools {
		a.allocateTier(plan, tierID, pool)
		a.setTierDiscovery(plan, tierID, pool)
	}
	return plan
}

type allocationCandidate struct {
	key     assignment.BindingKey
	floor   int
	ceiling int
}

type tierAllocation struct {
	tierID     assignment.TierID
	budget     int
	candidates []allocationCandidate
}

func (a *Allocator) allocateTier(plan *AllocationPlan, tierID assignment.TierID, pool *pool) {
	budget := pool.parallelism - pool.active
	if budget <= 0 {
		return
	}
	candidates := allocationCandidates(pool.order, pool.state, budget)
	capacities, _, visits := waterFill(candidates, budget, highestCeiling(candidates))
	plan.allocationVisits += visits
	for index, candidate := range candidates {
		a.setDesiredCapacity(plan, tierID, candidate.key, capacities[index])
	}
}

func (a *Allocator) allocateGlobal(plan *AllocationPlan) {
	budget := a.globalParallelism - a.activeCount
	if budget <= 0 {
		return
	}

	tiers := make([]tierAllocation, 0, len(a.pools))
	minimumLevel, maximumLevel := 0, 0
	haveCandidate := false
	for tierID, pool := range a.pools {
		tierBudget := pool.parallelism - pool.active
		if tierBudget <= 0 {
			continue
		}
		candidateBudget := budget
		if tierBudget < candidateBudget {
			candidateBudget = tierBudget
		}
		candidates := allocationCandidates(pool.order, pool.state, candidateBudget)
		if len(candidates) == 0 {
			continue
		}
		tiers = append(tiers, tierAllocation{tierID: tierID, budget: tierBudget, candidates: candidates})
		for _, candidate := range candidates {
			if !haveCandidate || candidate.floor < minimumLevel {
				minimumLevel = candidate.floor
			}
			if candidate.ceiling > maximumLevel {
				maximumLevel = candidate.ceiling
			}
			haveCandidate = true
		}
	}
	if !haveCandidate {
		return
	}

	visits := 0
	creditsAt := func(level int) int {
		total := 0
		for _, tier := range tiers {
			credits, inspected := creditsAtLevel(tier.candidates, level, tier.budget)
			visits += inspected
			if credits > tier.budget {
				credits = tier.budget
			}
			total += credits
			if total > budget {
				return budget + 1
			}
		}
		return total
	}

	level := maximumLevel
	if creditsAt(maximumLevel) > budget {
		low, high := minimumLevel, maximumLevel
		for low < high {
			middle := low + (high-low+1)/2
			if creditsAt(middle) <= budget {
				low = middle
			} else {
				high = middle - 1
			}
		}
		level = low
	}

	remainingByTier := make(map[assignment.TierID]int, len(tiers))
	used := 0
	for _, tier := range tiers {
		capacities, tierUsed, inspected := waterFill(tier.candidates, tier.budget, level)
		visits += inspected
		used += tierUsed
		remainingByTier[tier.tierID] = tier.budget - tierUsed
		for index, candidate := range tier.candidates {
			a.setDesiredCapacity(plan, tier.tierID, candidate.key, capacities[index])
		}
	}

	remaining := budget - used
	for _, key := range a.order {
		if remaining == 0 {
			break
		}
		tierID := a.tierOf[key]
		if remainingByTier[tierID] == 0 {
			continue
		}
		binding := a.binding(key)
		current := plan.desired[key]
		if binding.held || current != level || current >= binding.assignedDemand {
			continue
		}
		a.setDesiredCapacity(plan, tierID, key, current+1)
		remainingByTier[tierID]--
		remaining--
	}
	plan.allocationVisits += visits + len(a.order)
}

func allocationCandidates(
	order []assignment.BindingKey,
	state map[assignment.BindingKey]*binding,
	budget int,
) []allocationCandidate {
	candidates := make([]allocationCandidate, 0, len(order))
	for _, key := range order {
		binding := state[key]
		if binding.held || binding.assignedDemand <= binding.active {
			continue
		}
		headroom := binding.assignedDemand - binding.active
		if headroom > budget {
			headroom = budget
		}
		candidates = append(candidates, allocationCandidate{
			key: key, floor: binding.active, ceiling: binding.active + headroom,
		})
	}
	return candidates
}

// waterFill raises the least-filled candidates in batches. Its logarithmic
// factor is bounded by the configured capacity, not by raw provider demand.
func waterFill(candidates []allocationCandidate, budget, levelLimit int) ([]int, int, int) {
	capacities := make([]int, len(candidates))
	if len(candidates) == 0 || budget <= 0 {
		return capacities, 0, len(candidates)
	}
	minimumLevel, maximumLevel := candidates[0].floor, 0
	for index, candidate := range candidates {
		capacities[index] = candidate.floor
		if candidate.floor < minimumLevel {
			minimumLevel = candidate.floor
		}
		ceiling := candidate.ceiling
		if ceiling > levelLimit {
			ceiling = levelLimit
		}
		if ceiling > maximumLevel {
			maximumLevel = ceiling
		}
	}
	if maximumLevel < minimumLevel {
		return capacities, 0, len(candidates)
	}

	visits := len(candidates)
	usedAtLimit, inspected := creditsAtLevel(candidates, maximumLevel, budget)
	visits += inspected
	level := maximumLevel
	if usedAtLimit >= budget {
		low, high := minimumLevel, maximumLevel
		for low < high {
			middle := low + (high-low+1)/2
			used, count := creditsAtLevel(candidates, middle, budget)
			visits += count
			if used <= budget {
				low = middle
			} else {
				high = middle - 1
			}
		}
		level = low
	}

	used := 0
	for index, candidate := range candidates {
		target := candidate.ceiling
		if target > levelLimit {
			target = levelLimit
		}
		if target > level {
			target = level
		}
		if target < candidate.floor {
			target = candidate.floor
		}
		capacities[index] = target
		used += target - candidate.floor
	}
	visits += len(candidates)

	remaining := budget - used
	for index, candidate := range candidates {
		if remaining == 0 {
			break
		}
		ceiling := candidate.ceiling
		if ceiling > levelLimit {
			ceiling = levelLimit
		}
		if capacities[index] == level && capacities[index] < ceiling {
			capacities[index]++
			remaining--
			used++
		}
	}
	visits += len(candidates)
	return capacities, used, visits
}

func creditsAtLevel(candidates []allocationCandidate, level, stopAfter int) (int, int) {
	used := 0
	for index, candidate := range candidates {
		target := candidate.ceiling
		if target > level {
			target = level
		}
		if target > candidate.floor {
			used += target - candidate.floor
		}
		if used > stopAfter {
			return used, index + 1
		}
	}
	return used, len(candidates)
}

func highestCeiling(candidates []allocationCandidate) int {
	highest := 0
	for _, candidate := range candidates {
		if candidate.ceiling > highest {
			highest = candidate.ceiling
		}
	}
	return highest
}

func (a *Allocator) setDesiredCapacity(
	plan *AllocationPlan,
	tierID assignment.TierID,
	key assignment.BindingKey,
	capacity int,
) {
	previous := plan.desired[key]
	plan.desired[key] = capacity
	plan.byTier[tierID][key] = capacity
	plan.totalAdvertised += capacity - previous
}

func (a *Allocator) setTierDiscovery(plan *AllocationPlan, tierID assignment.TierID, pool *pool) {
	if len(pool.order) == 0 {
		return
	}
	holder := pool.order[pool.discovery]
	plan.discoveryByTier[tierID] = holder
	if plan.totalForTier(tierID) >= pool.parallelism {
		return
	}
	binding := pool.state[holder]
	if !binding.held && binding.assignedDemand == 0 && binding.active == 0 {
		a.setDesiredCapacity(plan, tierID, holder, 1)
	}
}

func (a *Allocator) setGlobalDiscovery(plan *AllocationPlan) {
	if len(a.order) == 0 {
		return
	}
	holder := a.order[a.discovery]
	tierID := a.tierOf[holder]
	plan.discoveryByTier[tierID] = holder
	pool := a.pools[tierID]
	if plan.totalAdvertised >= a.globalParallelism || plan.totalForTier(tierID) >= pool.parallelism {
		return
	}
	binding := pool.state[holder]
	if !binding.held && binding.assignedDemand == 0 && binding.active == 0 {
		a.setDesiredCapacity(plan, tierID, holder, 1)
	}
}

func (p *AllocationPlan) totalForTier(tierID assignment.TierID) int {
	total := 0
	for _, capacity := range p.byTier[tierID] {
		total += capacity
	}
	return total
}
