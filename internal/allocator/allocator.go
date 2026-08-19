// Package allocator distributes finite execution capacity among the
// bindings that draw on it. It does two jobs with one set of counts: it
// gates capsule launches to the admission budget (TryReserve/Release),
// and it computes the capacity each binding should advertise to the broker
// (Advertised).
//
// Capacity is credit, not entitlement. A tier holds exactly as many
// credits as its parallelism permits; a running capsule holds one; what is
// left is shared by demand. The sum of what every binding advertises never
// exceeds either its tier limit or the optional instance-wide limit.
//
// One discovery credit rotates per independent tier, or across the instance
// when a global limit is set. A silent binding announces one in turn and can
// therefore observe its queue. The rotation reaches every eligible binding
// within one pass. See
// docs/adrs/2026-08-13-admission-credits.md.
package allocator

import (
	"fmt"
	"sync"

	"github.com/rhobuild/runpool/internal/assignment"
)

// pool is one tier's parallelism budget and the bindings sharing it.
type pool struct {
	parallelism int
	order       []assignment.BindingKey // binding keys, registration order, stable
	state       map[assignment.BindingKey]*binding
	// discovery is the index in order of the binding currently holding
	// the discovery credit. Rotate advances it.
	discovery int
}

type binding struct {
	active int // capsules currently reserved/running for this binding
	// assignedDemand is what the provider has committed to this
	// binding: running plus waiting workloads, as its latest statistics
	// reported. It is demand already owed, not capacity on offer.
	assignedDemand int
	// held is true while this binding must not be offered new work —
	// disk pressure, quarantine, or any other contract that withholds
	// credit. Running capsules still count; only new capacity stops.
	held bool
}

type Allocator struct {
	mu sync.Mutex
	// globalParallelism is zero when tiers are intentionally independent.
	globalParallelism int
	pools             map[assignment.TierID]*pool
	tierOf            map[assignment.BindingKey]assignment.TierID
	order             []assignment.BindingKey // all binding keys, registration order
	discovery         int                     // instance-wide cursor when a global limit is set
	// held is the instance-wide hold. It is kept here, not only on the
	// bindings, because a hold is a statement about the instance and
	// outlives the set of bindings that existed when it was applied:
	// startup resumes a persisted emergency before the bindings are
	// registered, and a hold that only touched the current map would be
	// silently lost.
	held bool
}

// New creates an allocator whose tiers are independent.
func New() *Allocator { return NewWithGlobalParallelism(0) }

// NewWithGlobalParallelism creates an allocator with an instance-wide limit.
// Validation guarantees globalParallelism is positive; zero is reserved for
// the independent-tier behavior used by New.
func NewWithGlobalParallelism(globalParallelism int) *Allocator {
	return &Allocator{
		globalParallelism: globalParallelism,
		pools:             map[assignment.TierID]*pool{},
		tierOf:            map[assignment.BindingKey]assignment.TierID{},
	}
}

// Register adds a binding to its tier pool. parallelism is the pool size for the
// tier; every binding in a tier must pass the same value. More bindings
// than the limit is legal: they share the tier's credits and take turns at
// discovery rather than each reserving one.
func (a *Allocator) Register(tierID assignment.TierID, key assignment.BindingKey, parallelism int) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	p := a.pools[tierID]
	if p == nil {
		p = &pool{parallelism: parallelism, state: map[assignment.BindingKey]*binding{}}
		a.pools[tierID] = p
	}
	if p.parallelism != parallelism {
		return fmt.Errorf("tier %q registered with conflicting parallelism %d and %d", tierID, p.parallelism, parallelism)
	}
	if _, dup := p.state[key]; dup {
		return fmt.Errorf("binding %q already registered", key)
	}
	p.order = append(p.order, key)
	p.state[key] = &binding{held: a.held}
	a.tierOf[key] = tierID
	a.order = append(a.order, key)
	return nil
}

// SetAssignedDemand records what the provider has committed to a
// binding — running plus waiting workloads, from its latest statistics
// — which is what the free credits are shared by.
func (a *Allocator) SetAssignedDemand(key assignment.BindingKey, demand int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if b := a.binding(key); b != nil {
		if demand < 0 {
			demand = 0
		}
		b.assignedDemand = demand
	}
}

// Hold withholds new credit from every binding, or restores it. The disk
// monitor calls it when an emergency closes admission: capacity that
// cannot be served must not be announced, or the broker assigns work
// that then waits on a full disk. Running capsules are unaffected —
// they are already counted and cannot be retracted.
func (a *Allocator) Hold(held bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.held = held
	for _, p := range a.pools {
		for _, b := range p.state {
			b.held = held
		}
	}
}

// TryReserve claims one admission credit for key if its tier and the instance
// both have capacity, counting active capsules across the whole pool. It
// returns false when the pool is full, in which case the job waits in the
// caller's local pending queue. A successful reserve must be paired with Release.
func (a *Allocator) TryReserve(key assignment.BindingKey) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	p := a.poolOf(key)
	if p == nil {
		return false
	}
	if a.poolActive(p) >= p.parallelism {
		return false
	}
	if a.globalParallelism > 0 && a.active() >= a.globalParallelism {
		return false
	}
	p.state[key].active++
	return true
}

// Adopt accounts a lease that already exists at startup. It never
// refuses: the capsule is running and its resources are committed, so
// the only question is whether the books admit it.
//
// It reports whether that took the pool past its budget, which a restart
// does whenever parallelism was lowered while capsules were running.
// Until those leases release the pool holds more than its limit, and
// because advertised capacity is seeded from active, the invariant that
// the sum never exceeds parallelism is suspended for exactly that long.
// It converges on its own; what it must not do is converge silently,
// leaving an operator to meet it as a capacity figure that makes no
// sense. Reporting rather than logging keeps the decision here and the
// logger where the wiring is.
func (a *Allocator) Adopt(key assignment.BindingKey) (overBudget bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	b := a.binding(key)
	if b == nil {
		return false
	}
	b.active++
	// Both of the limits TryReserve gates on. A tier still inside its own
	// parallelism can put the instance past the one every tier shares, and
	// reporting only the tier's is how that arrives as an unexplained
	// capacity figure instead of a line.
	p := a.poolOf(key)
	if p != nil && a.poolActive(p) > p.parallelism {
		return true
	}
	return a.globalParallelism > 0 && a.active() > a.globalParallelism
}

// Release frees admission capacity when cleanup releases the lease.
func (a *Allocator) Release(key assignment.BindingKey) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if b := a.binding(key); b != nil && b.active > 0 {
		b.active--
	}
}

// Active reports a binding's current reserved/running capsule count.
func (a *Allocator) Active(key assignment.BindingKey) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	if b := a.binding(key); b != nil {
		return b.active
	}
	return 0
}

// Rotate advances every pool's discovery credit to the next binding that
// could use it: one with no demand signal and no running capsule. A
// binding that already has demand does not need discovery, so the
// rotation skips it, which is what bounds the wait — every silent
// binding holds the credit within one pass of the pool.
func (a *Allocator) Rotate() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.globalParallelism > 0 {
		a.discovery = a.nextDiscovery(a.order, a.discovery)
		return
	}
	for _, p := range a.pools {
		if len(p.order) == 0 {
			continue
		}
		p.discovery = a.nextDiscovery(p.order, p.discovery)
	}
}

// DiscoveryHolder reports which binding currently holds the tier's
// discovery credit — the one datum an operator needs to answer "why is
// that binding announcing zero".
func (a *Allocator) DiscoveryHolder(tierID assignment.TierID) assignment.BindingKey {
	a.mu.Lock()
	defer a.mu.Unlock()
	p := a.pools[tierID]
	if p == nil || len(p.order) == 0 {
		return ""
	}
	if a.globalParallelism > 0 {
		if len(a.order) == 0 {
			return ""
		}
		holder := a.order[a.discovery]
		if a.tierOf[holder] == tierID {
			return holder
		}
		return ""
	}
	return p.order[p.discovery]
}

// Advertised computes the capacity key should announce now. It is a pure
// function of pool state, so every binding's independent call sees the
// same distribution and the pool's advertised sum stays within its
// parallelism.
//
// The order is forced by what each part means. Running capsules cannot
// be retracted, so they are counted first. What remains is free credit,
// shared max-min fairly among bindings whose demand exceeds what they
// hold. Whatever is still unclaimed can fund the discovery credit, so a
// silent binding gets sight only out of genuinely idle capacity.
func (a *Allocator) Advertised(key assignment.BindingKey) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	p := a.poolOf(key)
	if p == nil {
		return 0
	}
	return a.distributeAll()[key]
}

// AdvertisedAll is the whole tier's distribution under one lock. The
// per-binding call is what a session announces, but the invariant is a
// statement about the pool, and reading it one binding at a time
// samples four different instants rather than one state.
func (a *Allocator) AdvertisedAll(tierID assignment.TierID) map[assignment.BindingKey]int {
	a.mu.Lock()
	defer a.mu.Unlock()
	p := a.pools[tierID]
	if p == nil {
		return nil
	}
	all := a.distributeAll()
	out := make(map[assignment.BindingKey]int, len(p.order))
	for _, key := range p.order {
		out[key] = all[key]
	}
	return out
}

// BindingCredit is one binding's line in the pool report: the demand it
// reported, the credits it holds, and what it would announce now.
type BindingCredit struct {
	Key            assignment.BindingKey
	AssignedDemand int
	Reserved       int
	Advertised     int
	Discovery      bool
}

// PoolReport is the tier's credit accounting, for operators and for the
// log line that explains why a binding announces what it does.
func (a *Allocator) PoolReport(tierID assignment.TierID) (parallelism int, rows []BindingCredit) {
	a.mu.Lock()
	defer a.mu.Unlock()
	p := a.pools[tierID]
	if p == nil {
		return 0, nil
	}
	adv := a.distributeAll()
	var discovery assignment.BindingKey
	if a.globalParallelism > 0 {
		if len(a.order) > 0 && a.tierOf[a.order[a.discovery]] == tierID {
			discovery = a.order[a.discovery]
		}
	} else if len(p.order) > 0 {
		discovery = p.order[p.discovery]
	}
	for _, k := range p.order {
		b := p.state[k]
		rows = append(rows, BindingCredit{
			Key: k, AssignedDemand: b.assignedDemand, Reserved: b.active,
			Advertised: adv[k], Discovery: k == discovery,
		})
	}
	return p.parallelism, rows
}

// CapacityReport is the instance-wide admission accounting. Parallelism is
// the effective limit: the configured global limit, or the sum of tier limits
// when tiers are independent. Global says which of those it is — the sum is
// arithmetic, not a limit anyone configured, and a reader deciding whether an
// instance-wide figure means anything needs to know the difference.
type CapacityReport struct {
	Parallelism int
	Active      int
	Advertised  int
	Available   int
	Global      bool
}

func (a *Allocator) CapacityReport() CapacityReport {
	a.mu.Lock()
	defer a.mu.Unlock()
	limit := a.globalParallelism
	global := limit > 0
	if limit == 0 {
		for _, p := range a.pools {
			limit += p.parallelism
		}
	}
	active := a.active()
	advertised := 0
	for _, n := range a.distributeAll() {
		advertised += n
	}
	available := limit - active
	if available < 0 {
		available = 0
	}
	return CapacityReport{
		Parallelism: limit, Active: active, Advertised: advertised,
		Available: available, Global: global,
	}
}

func (a *Allocator) distributeAll() map[assignment.BindingKey]int {
	if a.globalParallelism > 0 {
		return a.distributeGlobal()
	}
	out := make(map[assignment.BindingKey]int, len(a.order))
	for _, p := range a.pools {
		for key, credit := range a.distributeTier(p) {
			out[key] = credit
		}
	}
	return out
}

func (a *Allocator) distributeTier(p *pool) map[assignment.BindingKey]int {
	adv := make(map[assignment.BindingKey]int, len(p.order))
	for _, k := range p.order {
		adv[k] = p.state[k].active
	}
	remaining := p.parallelism - a.poolActive(p)
	if remaining < 0 {
		remaining = 0
	}

	// Water-fill: hand each free credit to the least-filled binding that
	// still wants more, breaking ties by registration order. Max-min
	// fair and deterministic — no cursor to make two callers disagree
	// about who got the credit.
	for remaining > 0 {
		var pick assignment.BindingKey
		best := int(^uint(0) >> 1)
		for _, k := range p.order {
			b := p.state[k]
			if !b.held && adv[k] < b.assignedDemand && adv[k] < best {
				pick, best = k, adv[k]
			}
		}
		if pick == "" {
			break
		}
		adv[pick]++
		remaining--
	}

	// The discovery credit: one, to one binding, only if a credit is
	// still unclaimed. A binding with demand or a running capsule does
	// not need it — it can already see and be seen.
	if remaining > 0 && len(p.order) > 0 {
		holder := p.order[p.discovery]
		if b := p.state[holder]; !b.held && b.assignedDemand == 0 && b.active == 0 {
			adv[holder] = 1
		}
	}
	return adv
}

func (a *Allocator) distributeGlobal() map[assignment.BindingKey]int {
	adv := make(map[assignment.BindingKey]int, len(a.order))
	tierAdvertised := make(map[assignment.TierID]int, len(a.pools))
	for _, key := range a.order {
		active := a.binding(key).active
		adv[key] = active
		tierAdvertised[a.tierOf[key]] += active
	}
	remaining := a.globalParallelism - a.active()
	if remaining < 0 {
		remaining = 0
	}

	for remaining > 0 {
		var pick assignment.BindingKey
		best := int(^uint(0) >> 1)
		for _, key := range a.order {
			b := a.binding(key)
			tierID := a.tierOf[key]
			if !b.held && tierAdvertised[tierID] < a.pools[tierID].parallelism &&
				adv[key] < b.assignedDemand && adv[key] < best {
				pick, best = key, adv[key]
			}
		}
		if pick == "" {
			break
		}
		adv[pick]++
		tierAdvertised[a.tierOf[pick]]++
		remaining--
	}

	if remaining > 0 && len(a.order) > 0 {
		holder := a.order[a.discovery]
		b := a.binding(holder)
		tierID := a.tierOf[holder]
		if !b.held && b.assignedDemand == 0 && b.active == 0 &&
			tierAdvertised[tierID] < a.pools[tierID].parallelism {
			adv[holder] = 1
		}
	}
	return adv
}

func (a *Allocator) nextDiscovery(order []assignment.BindingKey, current int) int {
	if len(order) == 0 {
		return 0
	}
	for i := 1; i <= len(order); i++ {
		next := (current + i) % len(order)
		b := a.binding(order[next])
		if b != nil && !b.held && b.assignedDemand == 0 && b.active == 0 {
			return next
		}
	}
	return current
}

func (a *Allocator) binding(key assignment.BindingKey) *binding {
	if p := a.poolOf(key); p != nil {
		return p.state[key]
	}
	return nil
}

func (a *Allocator) poolOf(key assignment.BindingKey) *pool { return a.pools[a.tierOf[key]] }

func (a *Allocator) poolActive(p *pool) int {
	total := 0
	for _, b := range p.state {
		total += b.active
	}
	return total
}

func (a *Allocator) active() int {
	total := 0
	for _, p := range a.pools {
		total += a.poolActive(p)
	}
	return total
}
