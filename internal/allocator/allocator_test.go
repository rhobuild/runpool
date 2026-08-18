package allocator

import (
	"sync"
	"testing"
)

func mustRegister(t *testing.T, a *Allocator, tier, key string, parallelism int) {
	t.Helper()
	if err := a.Register(tier, key, parallelism); err != nil {
		t.Fatal(err)
	}
}

// advertisedSum is the invariant's left-hand side. It reads the whole
// tier at once: summing per-binding calls would sample as many
// different instants as there are bindings, which says nothing about
// any single state of the pool.
func advertisedSum(a *Allocator, _ ...string) int {
	total := 0
	for _, v := range a.AdvertisedAll("std") {
		total += v
	}
	return total
}

// TestMoreBindingsThanParallelism: credits removed the floor, and with it the
// registration limit. Three bindings share two capacity units; nobody is
// rejected and the pool never promises more than it has.
func TestMoreBindingsThanParallelism(t *testing.T) {
	a := New()
	for _, key := range []string{"a", "b", "c"} {
		mustRegister(t, a, "std", key, 2)
	}
	if got := advertisedSum(a, "a", "b", "c"); got > 2 {
		t.Errorf("sum(advertised) = %d; the tier parallelism is 2", got)
	}
}

func TestRegisterRejectsConflictingParallelism(t *testing.T) {
	a := New()
	mustRegister(t, a, "std", "a", 2)
	if err := a.Register("std", "b", 3); err == nil {
		t.Error("a tier registered with conflicting parallelism should fail")
	}
}

// TestInvariantHoldsAcrossStates is the gate's first requirement, swept
// over the states a pool actually passes through: idle, partial demand,
// saturated demand, and running capsules.
func TestInvariantHoldsAcrossStates(t *testing.T) {
	keys := []string{"a", "b", "c"}
	for _, tc := range []struct {
		name        string
		parallelism int
		wants       []int
		holds       []int // capsules to reserve, per binding
	}{
		{"idle pool", 3, []int{0, 0, 0}, []int{0, 0, 0}},
		{"one binding wants everything", 3, []int{9, 0, 0}, []int{0, 0, 0}},
		{"all want everything", 3, []int{9, 9, 9}, []int{0, 0, 0}},
		{"demand plus running", 3, []int{9, 9, 0}, []int{1, 1, 0}},
		{"pool saturated by one", 2, []int{5, 5, 5}, []int{2, 0, 0}},
		{"more bindings than parallelism", 1, []int{0, 0, 0}, []int{0, 0, 0}},
		{"more bindings than parallelism, all busy", 1, []int{3, 3, 3}, []int{1, 0, 0}},
	} {
		a := New()
		for _, k := range keys {
			mustRegister(t, a, "std", k, tc.parallelism)
		}
		for i, k := range keys {
			a.SetAssignedDemand(k, tc.wants[i])
			for range tc.holds[i] {
				if !a.TryReserve(k) {
					t.Fatalf("%s: %s could not reserve within budget", tc.name, k)
				}
			}
		}
		if got := advertisedSum(a, keys...); got > tc.parallelism {
			t.Errorf("%s: sum(advertised) = %d > parallelism %d", tc.name, got, tc.parallelism)
		}
	}
}

// TestRunningCapsulesAreNeverRetracted: a job in flight cannot be taken
// back, so advertised is at least active — and the invariant still
// holds, because active is drawn from the same credits.
func TestRunningCapsulesAreNeverRetracted(t *testing.T) {
	a := New()
	mustRegister(t, a, "std", "a", 3)
	mustRegister(t, a, "std", "b", 3)
	for range 3 {
		if !a.TryReserve("a") {
			t.Fatal("a should claim capacity")
		}
	}
	a.SetAssignedDemand("a", 3)
	if got := a.Advertised("a"); got != 3 {
		t.Errorf("a advertised %d; want its 3 running capsules", got)
	}
	if got := a.Advertised("b"); got != 0 {
		t.Errorf("b advertised %d; the pool has no free credit to offer", got)
	}
	if a.TryReserve("b") {
		t.Fatal("pool full; b must wait")
	}
}

// TestSurplusFollowsDemand: free credits go to bindings that want them,
// max-min fairly, and a binding with no demand does not hold capacity
// hostage.
func TestSurplusFollowsDemand(t *testing.T) {
	a := New()
	mustRegister(t, a, "std", "a", 4)
	mustRegister(t, a, "std", "b", 4)
	mustRegister(t, a, "std", "c", 4)
	a.SetAssignedDemand("a", 10)
	a.SetAssignedDemand("b", 1)

	if got := a.Advertised("b"); got != 1 {
		t.Errorf("b advertised %d; want its single unit of demand", got)
	}
	if got := a.Advertised("a"); got != 3 {
		t.Errorf("a advertised %d; want the remaining credits", got)
	}
	if got := a.Advertised("c"); got != 0 {
		t.Errorf("c advertised %d; it has no demand and the credits are spoken for", got)
	}
}

func TestWaterFillIsMaxMin(t *testing.T) {
	a := New()
	mustRegister(t, a, "std", "a", 5)
	mustRegister(t, a, "std", "b", 5)
	mustRegister(t, a, "std", "c", 5)
	a.SetAssignedDemand("a", 10)
	a.SetAssignedDemand("b", 10)
	a.SetAssignedDemand("c", 1)

	got := map[string]int{"a": a.Advertised("a"), "b": a.Advertised("b"), "c": a.Advertised("c")}
	want := map[string]int{"a": 2, "b": 2, "c": 1}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("max-min share = %v; want %v", got, want)
			break
		}
	}
}

func TestDistributionIsConsistentAcrossCallers(t *testing.T) {
	a := New()
	mustRegister(t, a, "std", "a", 3)
	mustRegister(t, a, "std", "b", 3)
	a.SetAssignedDemand("a", 5)
	a.SetAssignedDemand("b", 5)
	for range 10 {
		if a.Advertised("a")+a.Advertised("b") > 3 {
			t.Fatal("two independent callers disagreed and oversubscribed the pool")
		}
	}
}

// TestDiscoveryCreditMakesASilentBindingVisible is the trapdoor the
// floor existed to close: a binding with no demand signal must
// eventually announce one, or it never learns it has queued work.
func TestDiscoveryCreditMakesASilentBindingVisible(t *testing.T) {
	a := New()
	mustRegister(t, a, "std", "a", 2)
	mustRegister(t, a, "std", "b", 2)
	mustRegister(t, a, "std", "c", 2)

	seen := map[string]bool{}
	// One pass of the pool must reach every silent binding: that is the
	// starvation bound, and it is what makes waiting for the credit
	// safe.
	for range 3 {
		for _, k := range []string{"a", "b", "c"} {
			if a.Advertised(k) > 0 {
				seen[k] = true
			}
		}
		a.Rotate()
	}
	for _, k := range []string{"a", "b", "c"} {
		if !seen[k] {
			t.Errorf("binding %q never held the discovery credit in a full pass; it would stay blind", k)
		}
	}
}

// TestDiscoveryIsNotOfferedToEveryoneAtOnce: the credit is one credit.
// Handing it to every silent binding simultaneously is the overshoot
// the floor caused.
func TestDiscoveryIsNotOfferedToEveryoneAtOnce(t *testing.T) {
	a := New()
	keys := []string{"a", "b", "c", "d"}
	for _, k := range keys {
		mustRegister(t, a, "std", k, 2)
	}
	holders := 0
	for _, k := range keys {
		if a.Advertised(k) > 0 {
			holders++
		}
	}
	if holders != 1 {
		t.Errorf("%d bindings announced capacity; the discovery credit is one", holders)
	}
}

// TestRotationSkipsBindingsThatCanAlreadySee: a binding with demand or a
// running capsule does not need discovery, and spending the rotation on
// it would lengthen every other binding's wait.
func TestRotationSkipsBindingsThatCanAlreadySee(t *testing.T) {
	a := New()
	for _, k := range []string{"a", "b", "c"} {
		mustRegister(t, a, "std", k, 3)
	}
	a.SetAssignedDemand("b", 5) // b has demand: it is visible without the credit

	held := map[string]int{}
	for range 6 {
		a.Rotate()
		held[a.DiscoveryHolder("std")]++
	}
	if held["b"] != 0 {
		t.Errorf("rotation gave the discovery credit to a binding with demand %d times", held["b"])
	}
	if held["a"] == 0 || held["c"] == 0 {
		t.Errorf("rotation starved a silent binding: %v", held)
	}
}

// TestDiscoveryNeedsAFreeCredit: sight is funded out of idle capacity,
// never out of a saturated pool, or the invariant would break exactly
// when the host is busiest.
func TestDiscoveryNeedsAFreeCredit(t *testing.T) {
	a := New()
	mustRegister(t, a, "std", "busy", 1)
	mustRegister(t, a, "std", "silent", 1)
	a.SetAssignedDemand("busy", 4)
	if !a.TryReserve("busy") {
		t.Fatal("busy should claim the only capacity unit")
	}
	if got := a.Advertised("silent"); got != 0 {
		t.Errorf("silent advertised %d with no free credit in the pool", got)
	}
	if got := advertisedSum(a, "busy", "silent"); got > 1 {
		t.Errorf("sum(advertised) = %d > parallelism 1", got)
	}
}

// TestHoldWithholdsNewCapacity: an emergency that closes admission must
// stop the broker assigning more work, while the capsules already
// running stay counted.
func TestHoldWithholdsNewCapacity(t *testing.T) {
	a := New()
	mustRegister(t, a, "std", "a", 3)
	mustRegister(t, a, "std", "b", 3)
	a.SetAssignedDemand("a", 5)
	if !a.TryReserve("a") {
		t.Fatal("a should reserve")
	}

	a.Hold(true)
	if got := a.Advertised("a"); got != 1 {
		t.Errorf("a advertised %d under hold; want only its running capsule", got)
	}
	if got := a.Advertised("b"); got != 0 {
		t.Errorf("b advertised %d under hold; want 0", got)
	}

	a.Hold(false)
	if got := a.Advertised("a"); got != 3 {
		t.Errorf("a advertised %d after the hold lifted; want its demand again", got)
	}
}

func TestPhysicalGate(t *testing.T) {
	a := New()
	mustRegister(t, a, "std", "a", 2)
	mustRegister(t, a, "std", "b", 2)
	for i := range 2 {
		if !a.TryReserve("a") {
			t.Fatalf("a should claim pool capacity unit %d", i+1)
		}
	}
	if a.TryReserve("b") {
		t.Fatal("pool is full; b must be refused")
	}
	a.Release("a")
	if !a.TryReserve("b") {
		t.Fatal("after a frees capacity, b should reserve")
	}
}

func TestReleaseFloorAtZero(t *testing.T) {
	a := New()
	mustRegister(t, a, "std", "a", 1)
	a.Release("a") // release with nothing active must not underflow
	if got := a.Active("a"); got != 0 {
		t.Errorf("active = %d; want 0", got)
	}
}

func TestConcurrentReserveWithinBudget(t *testing.T) {
	a := New()
	mustRegister(t, a, "std", "a", 4)
	mustRegister(t, a, "std", "b", 4)
	var wg sync.WaitGroup
	var mu sync.Mutex
	granted := 0
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if a.TryReserve("a") {
				mu.Lock()
				granted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if granted != 4 {
		t.Errorf("granted %d reservations; want exactly the parallelism budget of 4", granted)
	}
}

// TestNoOvershootUnderConcurrentTraffic: reserves, releases, demand
// updates and rotations run together while the invariant is sampled.
// This is the race the gate cares about — advertised is read by every
// binding's own loop while the pool moves under it.
func TestNoOvershootUnderConcurrentTraffic(t *testing.T) {
	a := New()
	keys := []string{"a", "b", "c", "d"}
	const parallelism = 3
	for _, k := range keys {
		mustRegister(t, a, "std", k, parallelism)
	}

	var workers sync.WaitGroup
	stop := make(chan struct{})
	for _, k := range keys {
		workers.Add(1)
		go func(k string) {
			defer workers.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				a.SetAssignedDemand(k, i%5)
				if a.TryReserve(k) {
					a.Release(k)
				}
				a.Rotate()
			}
		}(k)
	}

	// Sample the invariant while the pool moves underneath.
	for range 2000 {
		if got := advertisedSum(a, keys...); got > parallelism {
			t.Errorf("sum(advertised) = %d > parallelism %d", got, parallelism)
			break
		}
	}
	close(stop)
	workers.Wait()
}

func TestGlobalParallelismConstrainsEveryTier(t *testing.T) {
	a := NewWithGlobalParallelism(1)
	mustRegister(t, a, "small", "small-a", 1)
	mustRegister(t, a, "large", "large-a", 1)
	a.SetAssignedDemand("small-a", 1)
	a.SetAssignedDemand("large-a", 1)

	advertised := a.Advertised("small-a") + a.Advertised("large-a")
	if advertised != 1 {
		t.Fatalf("globally advertised capacity = %d; want 1", advertised)
	}
	if !a.TryReserve("small-a") {
		t.Fatal("first tier should reserve the global credit")
	}
	if a.TryReserve("large-a") {
		t.Fatal("second tier reserved beyond instance parallelism")
	}
	if got := a.CapacityReport(); got.Active != 1 || got.Available != 0 || got.Parallelism != 1 {
		t.Fatalf("capacity report = %+v", got)
	}
	a.Release("small-a")
	if !a.TryReserve("large-a") {
		t.Fatal("released global capacity was not reusable by another tier")
	}
}

func TestGlobalDiscoveryRotatesAcrossTiers(t *testing.T) {
	a := NewWithGlobalParallelism(1)
	mustRegister(t, a, "small", "small-a", 1)
	mustRegister(t, a, "large", "large-a", 1)

	seen := map[string]bool{}
	for range 2 {
		for _, key := range []string{"small-a", "large-a"} {
			if a.Advertised(key) == 1 {
				seen[key] = true
			}
		}
		if a.Advertised("small-a")+a.Advertised("large-a") > 1 {
			t.Fatal("global discovery oversubscribed advertised capacity")
		}
		a.Rotate()
	}
	if !seen["small-a"] || !seen["large-a"] {
		t.Fatalf("global discovery did not reach every tier: %v", seen)
	}
}

func TestAdoptedLeaseCountsAgainstGlobalParallelism(t *testing.T) {
	a := NewWithGlobalParallelism(1)
	mustRegister(t, a, "small", "small-a", 1)
	mustRegister(t, a, "large", "large-a", 1)
	a.Adopt("large-a")

	if a.TryReserve("small-a") {
		t.Fatal("startup reconciliation did not restore the global admission count")
	}
	if got := a.Advertised("small-a"); got != 0 {
		t.Fatalf("small tier advertised %d while an adopted lease holds global capacity", got)
	}
}

// TestGlobalRespectsTheSmallerTierCap: an instance limit does not let a tier
// exceed its own parallelism. With a global budget of three and a tier that
// only holds one, the spare credits must land in the tier that can take them
// rather than oversubscribing the small one.
func TestGlobalRespectsTheSmallerTierCap(t *testing.T) {
	a := NewWithGlobalParallelism(3)
	mustRegister(t, a, "small", "small-a", 1)
	mustRegister(t, a, "large", "large-a", 3)
	a.SetAssignedDemand("small-a", 5)
	a.SetAssignedDemand("large-a", 5)

	small, large := a.Advertised("small-a"), a.Advertised("large-a")
	if small != 1 {
		t.Errorf("small tier advertised %d; its own parallelism is 1", small)
	}
	if large != 2 {
		t.Errorf("large tier advertised %d; want the remaining global credit", large)
	}
	if got := a.CapacityReport(); got.Advertised != 3 || got.Parallelism != 3 {
		t.Errorf("capacity report = %+v; want three advertised of three", got)
	}

	// The small tier is full at one: its second reserve must be refused even
	// though the instance still has budget.
	if !a.TryReserve("small-a") {
		t.Fatal("the small tier should claim its single credit")
	}
	if a.TryReserve("small-a") {
		t.Error("the small tier reserved beyond its own parallelism")
	}
	if !a.TryReserve("large-a") {
		t.Error("the large tier should still claim global capacity")
	}
}

// TestCapacityReportSumsTiersWhenIndependent: without a global limit the
// effective instance parallelism is the sum of the tiers, which is the number
// `runpool status` shows an operator.
func TestCapacityReportSumsTiersWhenIndependent(t *testing.T) {
	a := New()
	mustRegister(t, a, "small", "small-a", 2)
	mustRegister(t, a, "large", "large-a", 3)

	if got := a.CapacityReport(); got.Parallelism != 5 || got.Active != 0 || got.Available != 5 {
		t.Fatalf("idle report = %+v; want parallelism 5, nothing active", got)
	}
	if !a.TryReserve("small-a") {
		t.Fatal("small tier should reserve")
	}
	if got := a.CapacityReport(); got.Parallelism != 5 || got.Active != 1 || got.Available != 4 {
		t.Fatalf("report after one reserve = %+v; want 1 active of 5", got)
	}
}

// TestNoGlobalOvershootUnderConcurrentTraffic is the global-mode counterpart
// of the per-tier race: the instance-wide sum is sampled while reserves,
// releases, demand updates and rotations run across two tiers at once.
// CapacityReport reads every tier under one lock, so the sample is a single
// state rather than one instant per tier.
func TestNoGlobalOvershootUnderConcurrentTraffic(t *testing.T) {
	const global = 3
	a := NewWithGlobalParallelism(global)
	keys := []string{"small-a", "small-b", "large-a"}
	mustRegister(t, a, "small", "small-a", 2)
	mustRegister(t, a, "small", "small-b", 2)
	mustRegister(t, a, "large", "large-a", 3)

	var workers sync.WaitGroup
	stop := make(chan struct{})
	for _, key := range keys {
		workers.Add(1)
		go func(key string) {
			defer workers.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				a.SetAssignedDemand(key, i%5)
				if a.TryReserve(key) {
					a.Release(key)
				}
				a.Rotate()
			}
		}(key)
	}

	for range 2000 {
		got := a.CapacityReport()
		if got.Advertised > global {
			t.Errorf("sum(advertised) = %d > instance parallelism %d", got.Advertised, global)
			break
		}
		if got.Active > global {
			t.Errorf("active = %d > instance parallelism %d", got.Active, global)
			break
		}
	}
	close(stop)
	workers.Wait()
}

// TestHoldSurvivesLaterRegistration is the startup ordering hole. The
// controller resumes a persisted disk emergency before it builds its
// bindings, so a hold that only walked the bindings registered at the time
// touched an empty map and was silently lost — leaving every binding
// advertising capacity into the very emergency that closed admission.
func TestHoldSurvivesLaterRegistration(t *testing.T) {
	a := New()
	a.Hold(true) // resumed before any binding exists

	mustRegister(t, a, "std", "a", 2)
	a.SetAssignedDemand("a", 5)
	if got := a.Advertised("a"); got != 0 {
		t.Errorf("a advertised %d under a hold applied before it registered; want 0", got)
	}

	a.Hold(false)
	if got := a.Advertised("a"); got != 2 {
		t.Errorf("a advertised %d after the hold lifted; want its demand", got)
	}

	// A binding registered while admission is open must not inherit a stale
	// hold either.
	a.Hold(true)
	a.Hold(false)
	mustRegister(t, a, "std", "b", 2)
	a.SetAssignedDemand("b", 5)
	if got := a.Advertised("b"); got == 0 {
		t.Error("b inherited a hold that had already been lifted")
	}
}

// TestPoolReportExplainsEveryBinding: the report is what an operator reads
// to answer "why is that binding announcing zero", so every column has to
// be right in both scheduling modes — and the discovery flag has to name
// exactly one holder, or the answer points at nobody.
func TestPoolReportExplainsEveryBinding(t *testing.T) {
	a := New()
	mustRegister(t, a, "std", "a", 3)
	mustRegister(t, a, "std", "b", 3)
	a.SetAssignedDemand("a", 5)
	if !a.TryReserve("a") {
		t.Fatal("a should reserve")
	}

	parallelism, rows := a.PoolReport("std")
	if parallelism != 3 {
		t.Errorf("parallelism = %d; want 3", parallelism)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d; want one per binding", len(rows))
	}
	if rows[0].Key != "a" || rows[0].AssignedDemand != 5 || rows[0].Reserved != 1 {
		t.Errorf("row a = %+v", rows[0])
	}
	if rows[0].Advertised < rows[0].Reserved {
		t.Errorf("row a advertised %d below its %d running", rows[0].Advertised, rows[0].Reserved)
	}
	// b is silent, so it is the one that can hold the discovery credit.
	if rows[1].Key != "b" || rows[1].AssignedDemand != 0 || rows[1].Reserved != 0 {
		t.Errorf("row b = %+v", rows[1])
	}
	holders := 0
	for _, r := range rows {
		if r.Discovery {
			holders++
		}
	}
	if holders != 1 {
		t.Errorf("%d bindings flagged as discovery holder; the credit is one", holders)
	}

	// An unknown tier is not an error with empty rows: it is no report.
	if p, rows := a.PoolReport("nope"); p != 0 || rows != nil {
		t.Errorf("unknown tier report = (%d, %v); want (0, nil)", p, rows)
	}
}

// TestPoolReportUnderAGlobalLimit: the discovery cursor is instance-wide in
// this mode, so a tier that does not hold it must say so rather than
// pointing at one of its own bindings.
func TestPoolReportUnderAGlobalLimit(t *testing.T) {
	a := NewWithGlobalParallelism(2)
	mustRegister(t, a, "small", "small-a", 1)
	mustRegister(t, a, "large", "large-a", 2)

	flagged := map[string]bool{}
	for _, tier := range []string{"small", "large"} {
		_, rows := a.PoolReport(tier)
		for _, r := range rows {
			if r.Discovery {
				flagged[r.Key] = true
			}
		}
		holder := a.DiscoveryHolder(tier)
		if holder != "" && !flagged[holder] {
			t.Errorf("tier %s reports holder %q that its own rows do not flag", tier, holder)
		}
	}
	if len(flagged) != 1 {
		t.Errorf("%d bindings flagged across every tier: %v; the global credit is one", len(flagged), flagged)
	}

	// A tier nobody registered has no holder.
	if got := a.DiscoveryHolder("absent"); got != "" {
		t.Errorf("DiscoveryHolder of an unknown tier = %q; want empty", got)
	}
}

// Active and the internal binding lookup have to tolerate a key that was
// never registered: the reporting paths call them with whatever the caller
// has, and a panic there takes down a serving loop.
func TestUnknownKeysAreAnsweredNotPanicked(t *testing.T) {
	a := New()
	mustRegister(t, a, "std", "a", 1)

	if got := a.Active("ghost"); got != 0 {
		t.Errorf("Active of an unregistered key = %d; want 0", got)
	}
	if got := a.Advertised("ghost"); got != 0 {
		t.Errorf("Advertised of an unregistered key = %d; want 0", got)
	}
	if a.TryReserve("ghost") {
		t.Error("an unregistered key reserved capacity")
	}
	if got := a.AdvertisedAll("absent"); got != nil {
		t.Errorf("AdvertisedAll of an unknown tier = %v; want nil", got)
	}
	// These must not panic and must not change the pool.
	a.SetAssignedDemand("ghost", 5)
	a.Release("ghost")
	a.Adopt("ghost")
	if got := a.CapacityReport(); got.Active != 0 {
		t.Errorf("an unregistered key changed the pool: %+v", got)
	}
}

// TestAdoptionOverBudgetIsReportedAndConverges is the invariant's blind
// spot.
//
// The sweep above only ever reserves, and reservation is bounded — so it
// proves the invariant for the path that cannot break it. Adoption can:
// it never refuses, because the capsule is already running and its
// resources are committed, so a restart after parallelism was lowered
// leaves the pool holding more than its limit. Advertised capacity is
// seeded from active, so the sum exceeds parallelism for exactly as long
// as those leases live.
//
// That is correct — retracting a running capsule is not an option — but
// it has to be said out loud and it has to end.
func TestAdoptionOverBudgetIsReportedAndConverges(t *testing.T) {
	a := New()
	keys := []string{"a", "b"}
	for _, k := range keys {
		mustRegister(t, a, "std", k, 1) // the tier shrank to one
	}

	// Two capsules survived the restart; the tier now allows one.
	if over := a.Adopt("a"); over {
		t.Error("the first adoption is within budget and must not report otherwise")
	}
	if over := a.Adopt("b"); !over {
		t.Fatal("adopting past the tier's parallelism was not reported; the advertised " +
			"total now exceeds the budget and nothing says why")
	}

	if got := advertisedSum(a, keys...); got <= 1 {
		t.Fatalf("sum(advertised) = %d; the point of this case is that it exceeds "+
			"parallelism while the adopted capsules run", got)
	}

	// Convergence: as the adopted leases release, the pool returns under
	// its budget without anything else intervening.
	a.Release("b")
	if got := advertisedSum(a, keys...); got > 1 {
		t.Errorf("after one release sum(advertised) = %d > parallelism 1; the pool "+
			"is not converging", got)
	}
	a.Release("a")
	if got := advertisedSum(a, keys...); got > 1 {
		t.Errorf("with nothing adopted sum(advertised) = %d > parallelism 1", got)
	}
}

// TestAdoptionOverTheInstanceBudgetIsReported is the same invariant seen
// from the other limit. Under scheduling.parallelism a tier can sit
// comfortably inside its own budget while the instance is past the one
// every tier shares, and TryReserve gates on both. An adoption that
// breaches only the shared limit is the case a tier-only check calls
// within budget - after which the capacity report shows more active than
// parallelism with nothing having said so.
func TestAdoptionOverTheInstanceBudgetIsReported(t *testing.T) {
	a := NewWithGlobalParallelism(2)
	mustRegister(t, a, "std", "a", 4) // roomy tier, tight instance
	mustRegister(t, a, "big", "b", 4)

	if over := a.Adopt("a"); over {
		t.Error("the first adoption is within both budgets and must not report otherwise")
	}
	if over := a.Adopt("b"); over {
		t.Error("the second adoption fills the instance limit exactly; that is not past it")
	}
	if over := a.Adopt("a"); !over {
		t.Fatal("adopting past scheduling.parallelism was not reported; every tier is " +
			"inside its own limit, so nothing else would say the instance is over")
	}

	report := a.CapacityReport()
	if !report.Global {
		t.Fatal("the report does not say the instance limit is configured; a reader " +
			"cannot tell it from the fallback sum")
	}
	if report.Active <= report.Parallelism {
		t.Fatalf("active %d against parallelism %d; the point of this case is that the "+
			"instance holds more than its limit while it converges",
			report.Active, report.Parallelism)
	}

	a.Release("a")
	if got := a.CapacityReport(); got.Active > got.Parallelism {
		t.Errorf("after one release active %d > parallelism %d; the instance is not converging",
			got.Active, got.Parallelism)
	}
}

// TestIndependentTiersReportNoInstanceLimit. Without scheduling.parallelism
// the report's Parallelism is the sum of tier limits - arithmetic, not a
// limit anyone configured - and Global is what tells a reader so. The
// adoption warning leans on it: printed as a limit, the sum sits beside a
// breach announcement contradicting it.
func TestIndependentTiersReportNoInstanceLimit(t *testing.T) {
	a := New()
	mustRegister(t, a, "std", "a", 2)
	mustRegister(t, a, "big", "b", 3)

	report := a.CapacityReport()
	if report.Global {
		t.Error("independent tiers report a configured instance limit; the figure is a sum")
	}
	if report.Parallelism != 5 {
		t.Errorf("effective parallelism = %d; want the sum of tiers, 5", report.Parallelism)
	}
}
