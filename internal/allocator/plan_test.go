package allocator

import (
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/rhobuild/runpool/internal/assignment"
)

func TestAllocationPlanIsCachedAndImmutable(t *testing.T) {
	allocator := New()
	mustRegister(t, allocator, "standard", "a", 2)
	mustRegister(t, allocator, "standard", "b", 2)
	allocator.SetAssignedDemand("a", 2)

	first := allocator.Plan()
	if again := allocator.Plan(); again != first {
		t.Fatal("two reads without an allocation change rebuilt the plan")
	}
	poll := allocator.BeginPoll("a")
	if !poll.Valid() {
		t.Fatal("the registered binding could not begin a poll")
	}
	if duringPoll := allocator.Plan(); duringPoll != first {
		t.Fatal("reserving a remote poll rebuilt an unchanged desired-capacity plan")
	}
	allocator.CompletePoll(poll, true, false)
	if afterPoll := allocator.Plan(); afterPoll != first {
		t.Fatal("acknowledging remote capacity rebuilt an unchanged desired-capacity plan")
	}

	allocator.SetAssignedDemand("a", 1)
	second := allocator.Plan()
	if second == first {
		t.Fatal("a demand change reused the preceding plan")
	}
	if second.Generation() <= first.Generation() {
		t.Fatalf("generation moved from %d to %d", first.Generation(), second.Generation())
	}
	if got := first.DesiredCapacity("a"); got != 2 {
		t.Fatalf("retained plan changed to capacity %d; want its original value 2", got)
	}
	if got := second.DesiredCapacity("a"); got != 1 {
		t.Fatalf("new plan capacity = %d; want 1", got)
	}

	copy := second.DesiredCapacityByTier("standard")
	copy["a"] = 99
	if got := second.DesiredCapacity("a"); got != 1 {
		t.Fatalf("mutating a returned tier map changed the immutable plan to %d", got)
	}
}

func TestRemoteCapacityCountersFollowSessionState(t *testing.T) {
	allocator := New()
	mustRegister(t, allocator, "standard", "a", 2)
	mustRegister(t, allocator, "standard", "b", 2)
	allocator.SetAssignedDemand("a", 2)

	poll := allocator.BeginPoll("a")
	if got := poll.Capacity(); got != 2 {
		t.Fatalf("poll capacity = %d; want 2", got)
	}
	if got := allocator.CapacityReport().RemoteCapacity; got != 2 {
		t.Fatalf("in-flight remote capacity = %d; want 2", got)
	}
	allocator.CompletePoll(poll, true, false)
	if got := allocator.CapacityReport().RemoteCapacity; got != 2 {
		t.Fatalf("published remote capacity = %d; want 2", got)
	}
	allocator.SessionClosed("a", false)
	if got := allocator.CapacityReport().RemoteCapacity; got != 2 {
		t.Fatalf("uncertain close released remote capacity: got %d, want 2", got)
	}
	allocator.SessionOpened("a")
	if got := allocator.CapacityReport().RemoteCapacity; got != 0 {
		t.Fatalf("replacement session left remote capacity at %d; want 0", got)
	}
}

func TestBatchAllocationMatchesTheCreditAtATimeContract(t *testing.T) {
	random := rand.New(rand.NewPCG(0x72756e706f6f6c, 0x616c6c6f63617465))
	for iteration := range 1_000 {
		global := iteration%2 == 0
		allocator := randomizedAllocator(t, random, global)
		want := referenceDistribution(allocator)
		got := allocator.Plan().desired
		if len(got) != len(want) {
			t.Fatalf("iteration %d: plan has %d bindings; reference has %d", iteration, len(got), len(want))
		}
		for key, capacity := range want {
			if got[key] != capacity {
				t.Fatalf("iteration %d global=%t binding=%q: batch=%d reference=%d\nstate=%s",
					iteration, global, key, got[key], capacity, describeAllocator(allocator))
			}
		}
	}
}

func TestConcurrentReadersObserveCompletePlans(t *testing.T) {
	allocator := New()
	const parallelism = 64
	for index := range 128 {
		key := assignment.BindingKey(bindingName(index))
		mustRegister(t, allocator, "standard", assignment.SourceWorkloadKey(key), parallelism)
		allocator.SetAssignedDemand(key, parallelism)
	}

	var wait sync.WaitGroup
	start := make(chan struct{})
	for range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			var previous uint64
			for range 500 {
				plan := allocator.Plan()
				if plan.Generation() < previous {
					t.Errorf("plan generation moved backward from %d to %d", previous, plan.Generation())
					return
				}
				previous = plan.Generation()
				total := 0
				for _, capacity := range plan.DesiredCapacityByTier("standard") {
					total += capacity
				}
				if total > parallelism {
					t.Errorf("generation %d advertises %d with parallelism %d", plan.Generation(), total, parallelism)
					return
				}
			}
		}()
	}
	close(start)
	for iteration := range 500 {
		key := assignment.BindingKey(bindingName(iteration % 128))
		allocator.SetAssignedDemand(key, iteration%parallelism)
	}
	wait.Wait()
}

func randomizedAllocator(t *testing.T, random *rand.Rand, global bool) *Allocator {
	t.Helper()
	tierCount := 1 + random.IntN(4)
	parallelism := make([]int, tierCount)
	totalParallelism := 0
	for tier := range tierCount {
		parallelism[tier] = 1 + random.IntN(8)
		totalParallelism += parallelism[tier]
	}
	allocator := New()
	if global {
		allocator = NewWithGlobalParallelism(1 + random.IntN(totalParallelism))
	}
	keys := make([]assignment.BindingKey, 0, tierCount*4)
	for tier := range tierCount {
		bindingCount := 1 + random.IntN(6)
		for binding := range bindingCount {
			key := assignment.BindingKey(bindingName(tier*10 + binding))
			if err := allocator.Register(assignment.TierID(bindingName(tier)), key, parallelism[tier]); err != nil {
				t.Fatal(err)
			}
			allocator.SessionOpened(key)
			allocator.SetAssignedDemand(key, random.IntN(13))
			keys = append(keys, key)
		}
	}
	for range random.IntN(totalParallelism + 1) {
		allocator.TryReserve(keys[random.IntN(len(keys))])
	}
	if random.IntN(10) == 0 {
		allocator.Hold(true)
	}
	return allocator
}

func referenceDistribution(allocator *Allocator) map[assignment.BindingKey]int {
	if allocator.globalParallelism > 0 {
		return referenceGlobalDistribution(allocator)
	}
	out := make(map[assignment.BindingKey]int, len(allocator.order))
	for _, pool := range allocator.pools {
		for key, capacity := range referenceTierDistribution(pool) {
			out[key] = capacity
		}
	}
	return out
}

func referenceTierDistribution(pool *pool) map[assignment.BindingKey]int {
	desired := make(map[assignment.BindingKey]int, len(pool.order))
	for _, key := range pool.order {
		desired[key] = pool.state[key].active
	}
	remaining := pool.parallelism - pool.active
	if remaining < 0 {
		remaining = 0
	}
	for remaining > 0 {
		var selected assignment.BindingKey
		best := int(^uint(0) >> 1)
		for _, key := range pool.order {
			binding := pool.state[key]
			if !binding.held && desired[key] < binding.assignedDemand && desired[key] < best {
				selected, best = key, desired[key]
			}
		}
		if selected == "" {
			break
		}
		desired[selected]++
		remaining--
	}
	if remaining > 0 && len(pool.order) > 0 {
		holder := pool.order[pool.discovery]
		binding := pool.state[holder]
		if !binding.held && binding.assignedDemand == 0 && binding.active == 0 {
			desired[holder] = 1
		}
	}
	return desired
}

func referenceGlobalDistribution(allocator *Allocator) map[assignment.BindingKey]int {
	desired := make(map[assignment.BindingKey]int, len(allocator.order))
	tierTotals := make(map[assignment.TierID]int, len(allocator.pools))
	for _, key := range allocator.order {
		active := allocator.binding(key).active
		desired[key] = active
		tierTotals[allocator.tierOf[key]] += active
	}
	remaining := allocator.globalParallelism - allocator.activeCount
	if remaining < 0 {
		remaining = 0
	}
	for remaining > 0 {
		var selected assignment.BindingKey
		best := int(^uint(0) >> 1)
		for _, key := range allocator.order {
			binding := allocator.binding(key)
			tierID := allocator.tierOf[key]
			if !binding.held && tierTotals[tierID] < allocator.pools[tierID].parallelism &&
				desired[key] < binding.assignedDemand && desired[key] < best {
				selected, best = key, desired[key]
			}
		}
		if selected == "" {
			break
		}
		desired[selected]++
		tierTotals[allocator.tierOf[selected]]++
		remaining--
	}
	if remaining > 0 && len(allocator.order) > 0 {
		holder := allocator.order[allocator.discovery]
		binding := allocator.binding(holder)
		tierID := allocator.tierOf[holder]
		if !binding.held && binding.assignedDemand == 0 && binding.active == 0 &&
			tierTotals[tierID] < allocator.pools[tierID].parallelism {
			desired[holder] = 1
		}
	}
	return desired
}

func describeAllocator(allocator *Allocator) string {
	var description strings.Builder
	for _, key := range allocator.order {
		binding := allocator.binding(key)
		fmt.Fprintf(&description, "%s/%s active=%d demand=%d held=%t; ",
			allocator.tierOf[key], key, binding.active, binding.assignedDemand, binding.held)
	}
	return description.String()
}

func bindingName(index int) string { return strconv.Itoa(index) }
