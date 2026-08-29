// Package allocator distributes finite execution capacity among the
// bindings that draw on it. It does two jobs with one set of counts: it
// gates capsule launches to the admission budget (TryReserve/Release),
// and it computes the capacity each binding should advertise to the broker
// (Advertised).
//
// Capacity is credit, not entitlement. A tier holds exactly as many
// credits as its parallelism permits; a running capsule holds one; what is
// left is shared by demand. Poll reservations keep both the desired
// distribution and the capacity that may still be in force remotely within
// the tier limit and the optional instance-wide limit.
//
// One discovery credit rotates per independent tier, or across the instance
// when a global limit is set. Only the holder's successful empty poll advances
// it, and the preceding holder must confirm zero before the next can publish
// the same credit. A silent binding therefore observes its queue without two
// sessions spending one unit concurrently. See
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
	// the discovery credit. discoveryGeneration changes with the holder,
	// so a late result from an earlier poll cannot move it again.
	discovery           int
	discoveryGeneration uint64
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

	// published is the last capacity a successful provider poll confirmed.
	// A pending poll is counted at the larger of its capacity and published:
	// until its outcome is known, either value may be in force remotely.
	published int
	pending   *pendingPoll
	pollID    uint64
	// remoteKnown is false on startup and after an uncertain session close.
	// A newly opened session proves the predecessor is gone and starts at
	// zero, which is when this becomes true.
	remoteKnown bool
}

type pendingPoll struct {
	id       uint64
	capacity int
}

// Poll is one capacity announcement reserved against the remote budget.
// Its fields are private so only BeginPoll can create a valid value; the
// caller carries it unchanged to CompletePoll after the provider answers.
type Poll struct {
	key                 assignment.BindingKey
	id                  uint64
	capacity            int
	discovery           bool
	discoveryGeneration uint64
}

// Capacity is the value this poll must announce to the provider.
func (p Poll) Capacity() int { return p.capacity }

// Valid reports whether BeginPoll reserved this poll against a registered
// binding. The serving loop must not contact the provider for an invalid poll.
func (p Poll) Valid() bool { return p.id != 0 }

type Allocator struct {
	mu sync.Mutex
	// globalParallelism is zero when tiers are intentionally independent.
	globalParallelism   int
	pools               map[assignment.TierID]*pool
	tierOf              map[assignment.BindingKey]assignment.TierID
	order               []assignment.BindingKey // all binding keys, registration order
	discovery           int                     // instance-wide cursor when a global limit is set
	discoveryGeneration uint64
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
	// Held is checked here and not only where capacity is advertised.
	// Withholding what the broker is offered stops work being assigned;
	// it does not stop a pass that is already looping over a batch of
	// ready attempts, which reads the pressure once before the loop. A
	// hold that lands underneath that loop has to be obeyed by the
	// admission itself, or the capsules it starts land on the filesystem
	// the emergency is about.
	if b := p.state[key]; b == nil || b.held {
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

// SessionOpened records that the provider accepted a new session for key.
// The provider refuses a second live session for the same binding, so this
// proves any predecessor is gone and its capacity has returned to zero.
func (a *Allocator) SessionOpened(key assignment.BindingKey) {
	a.mu.Lock()
	defer a.mu.Unlock()
	b := a.binding(key)
	if b == nil {
		return
	}
	b.published = 0
	b.pending = nil
	b.remoteKnown = true
}

// SessionClosed records the outcome of closing a provider session. A
// confirmed close releases its published capacity. An uncertain close keeps
// the last possible value reserved and makes the pool wait for a replacement
// session before increasing any announcement.
func (a *Allocator) SessionClosed(key assignment.BindingKey, confirmed bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	b := a.binding(key)
	if b == nil {
		return
	}
	if b.pending != nil && b.pending.capacity > b.published {
		b.published = b.pending.capacity
	}
	b.pending = nil
	if confirmed {
		b.published = 0
		b.remoteKnown = true
		return
	}
	b.remoteKnown = false
}

// BeginPoll reserves one provider announcement. Increases are clamped against
// the capacities that other sessions may still hold; decreases do not free
// their old credit until CompletePoll confirms the provider accepted them.
//
// A zero-capacity poll is returned for an unknown key or while a predecessor
// session in the same budget is still unresolved. A binding loop has at most
// one poll in flight, so a second BeginPoll before completion also fails
// closed at zero.
func (a *Allocator) BeginPoll(key assignment.BindingKey) Poll {
	a.mu.Lock()
	defer a.mu.Unlock()
	b := a.binding(key)
	if b == nil || b.pending != nil {
		return Poll{key: key}
	}

	desired := 0
	if a.remoteStateKnown(key) {
		desired = a.distributeAll()[key]
	}
	capacity := a.clampPublished(key, desired)
	discovery, generation := a.discoveryAnnouncement(key, capacity)

	b.pollID++
	b.pending = &pendingPoll{id: b.pollID, capacity: capacity}
	return Poll{
		key: key, id: b.pollID, capacity: capacity, discovery: discovery,
		discoveryGeneration: generation,
	}
}

// CompletePoll records which capacity the provider may now hold and advances
// discovery only when the current holder's own poll completed empty. Failed
// polls retain the larger value because the request may have reached the
// provider even when its response did not reach the controller.
func (a *Allocator) CompletePoll(poll Poll, succeeded, empty bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	b := a.binding(poll.key)
	if b == nil || b.pending == nil || b.pending.id != poll.id {
		return
	}
	if succeeded {
		b.published = poll.capacity
	} else if poll.capacity > b.published {
		b.published = poll.capacity
	}
	b.pending = nil
	if !succeeded || !empty || !poll.discovery {
		return
	}
	a.rotateDiscovery(poll)
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

// Advertised computes the desired capacity for key. BeginPoll turns that
// desired value into the safe announcement after accounting for values that
// concurrent provider sessions may still hold remotely.
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

// AdvertisedAll is the whole tier's desired distribution under one lock. The
// desired invariant is a statement about the pool; reading one binding at a
// time samples different instants and proves nothing about a single state.
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

// BindingCredit is one binding's line in the pool report: its demand, local
// reservations, desired announcement and maximum capacity that may currently
// be in force remotely.
type BindingCredit struct {
	Key            assignment.BindingKey
	AssignedDemand int
	Reserved       int
	Advertised     int
	RemoteCapacity int
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
			Advertised: adv[k], RemoteCapacity: a.effectivePublished(b), Discovery: k == discovery,
		})
	}
	return p.parallelism, rows
}

// CapacityReport is the instance-wide admission accounting. RemoteCapacity
// includes in-flight announcements conservatively. Parallelism is the
// configured global limit, or the sum of independent tier limits; Global says
// which one it is.
type CapacityReport struct {
	Parallelism    int
	Active         int
	Advertised     int
	RemoteCapacity int
	Available      int
	Global         bool
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
	remoteCapacity := 0
	for _, key := range a.order {
		remoteCapacity += a.effectivePublished(a.binding(key))
	}
	available := limit - active
	if available < 0 {
		available = 0
	}
	return CapacityReport{
		Parallelism: limit, Active: active, Advertised: advertised, RemoteCapacity: remoteCapacity,
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

// remoteStateKnown reports whether every predecessor session sharing key's
// capacity budget has been accounted for. Independent tiers do not block one
// another; a global limit makes every binding share one remote budget.
func (a *Allocator) remoteStateKnown(key assignment.BindingKey) bool {
	if a.globalParallelism > 0 {
		for _, candidate := range a.order {
			if b := a.binding(candidate); b != nil && !b.remoteKnown {
				return false
			}
		}
		return true
	}
	p := a.poolOf(key)
	if p == nil {
		return false
	}
	for _, candidate := range p.order {
		if !p.state[candidate].remoteKnown {
			return false
		}
	}
	return true
}

// clampPublished limits one new announcement by every capacity value that may
// still be in force remotely. The pending maximum is reserved before the
// network call, so concurrent bindings cannot each spend the same credit.
func (a *Allocator) clampPublished(key assignment.BindingKey, desired int) int {
	if desired <= 0 {
		return 0
	}
	p := a.poolOf(key)
	if p == nil {
		return 0
	}
	tierAvailable := p.parallelism
	for _, candidate := range p.order {
		if candidate != key {
			tierAvailable -= a.effectivePublished(p.state[candidate])
		}
	}
	if tierAvailable < 0 {
		tierAvailable = 0
	}
	allowed := tierAvailable
	if a.globalParallelism > 0 {
		globalAvailable := a.globalParallelism
		for _, candidate := range a.order {
			if candidate != key {
				globalAvailable -= a.effectivePublished(a.binding(candidate))
			}
		}
		if globalAvailable < 0 {
			globalAvailable = 0
		}
		if globalAvailable < allowed {
			allowed = globalAvailable
		}
	}
	if desired < allowed {
		return desired
	}
	return allowed
}

func (a *Allocator) effectivePublished(b *binding) int {
	if b == nil {
		return 0
	}
	capacity := b.published
	if b.pending != nil && b.pending.capacity > capacity {
		capacity = b.pending.capacity
	}
	return capacity
}

func (a *Allocator) discoveryAnnouncement(key assignment.BindingKey, capacity int) (bool, uint64) {
	if capacity == 0 {
		return false, 0
	}
	b := a.binding(key)
	if b == nil || b.held || b.assignedDemand != 0 || b.active != 0 {
		return false, 0
	}
	if a.globalParallelism > 0 {
		if len(a.order) == 0 || a.order[a.discovery] != key {
			return false, 0
		}
		return true, a.discoveryGeneration
	}
	p := a.poolOf(key)
	if p == nil || len(p.order) == 0 || p.order[p.discovery] != key {
		return false, 0
	}
	return true, p.discoveryGeneration
}

func (a *Allocator) rotateDiscovery(poll Poll) {
	if a.globalParallelism > 0 {
		if poll.discoveryGeneration != a.discoveryGeneration || len(a.order) == 0 ||
			a.order[a.discovery] != poll.key {
			return
		}
		a.discovery = a.nextDiscovery(a.order, a.discovery)
		a.discoveryGeneration++
		return
	}
	p := a.poolOf(poll.key)
	if p == nil || poll.discoveryGeneration != p.discoveryGeneration ||
		len(p.order) == 0 || p.order[p.discovery] != poll.key {
		return
	}
	p.discovery = a.nextDiscovery(p.order, p.discovery)
	p.discoveryGeneration++
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
