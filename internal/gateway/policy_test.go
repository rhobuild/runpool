package gateway

import (
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rhobuild/runpool/internal/egress"
	"github.com/rhobuild/runpool/internal/platform/atomicfile"
)

// TestPolicyGenerationAdvancesOnlyOnChange: the relay drops pooled
// connections when the policy moves, because a connection authorised under
// the previous one is reused without ever reaching the dialer where the
// address check lives. That hinges on the counter advancing exactly when a
// new policy takes effect — never on an unchanged re-read, or every request
// would tear down the pool.
func TestPolicyGenerationAdvancesOnlyOnChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.json")
	write := func(deny string) {
		body := `{"internal_subnet":"172.31.0.0/24","uplink_subnet":"172.31.1.0/24",` +
			`"allow":[],"deny":["` + deny + `"]}`
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("10.0.0.0/8")

	s := &PolicyStore{Path: path}
	if _, err := s.Current(); err != nil {
		t.Fatal(err)
	}
	first := s.Generation()
	if first == 0 {
		t.Fatal("generation stayed zero after the first policy was installed")
	}

	// An unchanged file is served from cache and must not advance.
	for range 3 {
		if _, err := s.Current(); err != nil {
			t.Fatal(err)
		}
	}
	if got := s.Generation(); got != first {
		t.Errorf("generation moved to %d on an unchanged policy; the pool would be dropped every request", got)
	}

	// A restriction that actually changed must advance it.
	time.Sleep(10 * time.Millisecond) // distinct mtime
	write("192.168.0.0/16")
	if _, err := s.Current(); err != nil {
		t.Fatal(err)
	}
	if got := s.Generation(); got == first {
		t.Error("generation did not move on a new policy; connections pooled under the old one would keep it")
	}
}

// TestRelayNoticesAPolicyChangeWithoutDialing is the regression this fix
// first got wrong. The generation only advances when Current() re-reads the
// file, and Current() is otherwise reached only from dial() — which a
// request served from the connection pool never calls. Checking the
// generation without reading the policy therefore could not detect a change
// in precisely the case the pool-expiry exists for.
func TestRelayNoticesAPolicyChangeWithoutDialing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.json")
	write := func(deny string) {
		body := `{"internal_subnet":"172.31.0.0/24","uplink_subnet":"172.31.1.0/24",` +
			`"allow":[],"deny":["` + deny + `"]}`
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("10.0.0.0/8")

	r := &Relay{Policy: &PolicyStore{Path: path}, Log: discardLogger()}

	// Two observations settle the baseline; no dial happens in either.
	r.transportInForce()
	baseline := r.transportInForce().gen
	if baseline == 0 {
		t.Fatal("the relay never read the policy; a pooled request would keep the old one forever")
	}

	if got := r.transportInForce().gen; got != baseline {
		t.Errorf("generation moved to %d with an unchanged policy; the pool would be dropped every request", got)
	}

	write("192.168.0.0/16")
	if got := r.transportInForce().gen; got == baseline {
		t.Error("the relay did not notice a new policy without dialing; pooled connections would keep the old one")
	}
}

// TestAWideningAllowIsRefusedOnTheReloadChannel: the rule that an allow
// may not be broader than a range the baseline withholds is a property
// of the policy, not of the configuration file.
//
// Allow is consulted before deny, so such an entry reopens the whole
// withheld range. A gateway takes a policy from this channel as well as
// from configuration, and a check that lived only at the configuration
// entry point left this one open.
func TestAWideningAllowIsRefusedOnTheReloadChannel(t *testing.T) {
	dir := t.TempDir()
	path := PolicyPath(dir)
	inForce := `{"internal_subnet":"172.31.0.0/24","uplink_subnet":"172.31.1.0/24",` +
		`"allow":[],"deny":["10.0.0.0/8"]}`
	if err := os.WriteFile(path, []byte(inForce), 0o600); err != nil {
		t.Fatal(err)
	}

	incoming := `{"allow":["0.0.0.0/0"],"deny":["10.0.0.0/8"]}`
	err := Reload(dir, strings.NewReader(incoming))
	if err == nil {
		t.Fatal("the reload channel accepted an allow that reopens the whole space")
	}
	// The reason matters, not just the refusal: this host has no legs
	// and no NET_ADMIN, so the kernel step refuses everything that
	// reaches it. A test satisfied by any error would pass with the rule
	// removed.
	if !strings.Contains(err.Error(), "broader than a range") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != inForce {
		t.Errorf("the refused policy was written anyway:\n%s", got)
	}
}

// TestAConcurrentInstallLeavesAPolicyInForce: installers racing each
// other leave the relay a policy it can read, at every moment and at the
// end.
//
// This is the composition, not either mechanism. Two things hold it up —
// the kernel lock that orders the installers and the unique temporary
// name that makes each write atomic — and this test passes as long as
// one of them does, which is exactly why it cannot stand for either.
// TestConcurrentInstallsAreSerialized covers the lock, and the write is
// covered where it lives, by atomicfile's own
// TestAReplacedFileIsNeverReadHalfWritten; each fails on its own
// mechanism alone.
func TestAConcurrentInstallLeavesAPolicyInForce(t *testing.T) {
	dir := t.TempDir()
	path := PolicyPath(dir)
	inForce := `{"internal_subnet":"172.31.0.0/24","uplink_subnet":"172.31.1.0/24",` +
		`"allow":[],"deny":["10.0.0.0/8"]}`
	if err := os.WriteFile(path, []byte(inForce), 0o600); err != nil {
		t.Fatal(err)
	}

	// A reader racing the writers: every intermediate state it observes
	// must be a policy, never a splice.
	stop := make(chan struct{})
	bad := make(chan string, 1)
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				continue // the rename is atomic; a missing file is not
			}
			if _, err := ParsePolicy(string(raw)); err != nil {
				select {
				case bad <- string(raw):
				default:
				}
				return
			}
		}
	}()

	// The kernel step is stubbed: this host has neither leg nor
	// NET_ADMIN, and with the real one every install would refuse before
	// writing anything — leaving the test passing over a file nobody
	// ever touched.
	noKernel := func(egress.Policy) error { return nil }
	reload := func() error {
		return installPolicyWith(dir, func(current egress.Policy) (egress.Policy, error) {
			next := current
			next.Deny = []string{"10.0.0.0/8", "192.168.0.0/16"}
			return next, next.Validate()
		}, noKernel)
	}
	denyAll := func() error {
		return installPolicyWith(dir, func(current egress.Policy) (egress.Policy, error) {
			next := current
			next.Allow, next.Deny = nil, []string{"0.0.0.0/0"}
			return next, nil
		}, noKernel)
	}

	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if i%2 == 0 {
				_ = reload()
				return
			}
			_ = denyAll()
		}()
	}
	wg.Wait()
	close(stop)

	select {
	case raw := <-bad:
		t.Fatalf("a reader observed a policy it could not parse:\n%s", raw)
	default:
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParsePolicy(string(raw)); err != nil {
		t.Fatalf("the policy left in force does not parse: %v\n%s", err, raw)
	}
}

// TestAPolicyMoveRetiresTheTransport: when the policy moves, the pool
// that was authorised under the old one must stop being reachable.
//
// Closing the idle connections is not that. By contract it leaves a
// connection carrying a request alone, and that connection goes back
// into the pool afterwards, where the next request reuses it without
// reaching the dialer — which is where the address is checked. The
// kernel does not catch it either: the ruleset accepts established
// traffic ahead of every reject. Replacing the transport is what makes
// the old pool unreachable whatever state its connections are in.
func TestAPolicyMoveRetiresTheTransport(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.json")
	write := func(deny string) {
		body := `{"internal_subnet":"172.31.0.0/24","uplink_subnet":"172.31.1.0/24",` +
			`"allow":[],"deny":["` + deny + `"]}`
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("10.0.0.0/8")

	r := &Relay{Policy: &PolicyStore{Path: path}, Log: discardLogger()}
	first := r.transportInForce()

	// A second observation of the same policy is not a move.
	if got := r.transportInForce(); got != first {
		t.Fatal("the transport was replaced without the policy moving; every request would start a new pool")
	}

	write("192.168.0.0/16")
	if got := r.transportInForce(); got == first {
		t.Error("the transport survived a policy move; a connection pooled under the old one " +
			"still carries the next request past the dialer, where the address is checked")
	}
}

// TestConcurrentInstallsAreSerialized: two installers never occupy the
// critical section at once.
//
// A reload and an emergency close arrive as separate `docker exec`
// processes into the same container, so nothing in the program's own
// memory can order them — it has to be a lock the kernel holds. Without
// one they both read the same policy in force and each builds its
// successor from a document the other is about to replace, so the second
// rename silently discards the first install's decision.
//
// This is deliberately not the same test as the one above. A unique
// temporary name alone keeps every observable state parseable, so a
// splice test passes whether or not the lock is there, and for a while
// one test stood for both mechanisms while only one of them was really
// covered.
func TestConcurrentInstallsAreSerialized(t *testing.T) {
	dir := t.TempDir()
	inForce := `{"internal_subnet":"172.31.0.0/24","uplink_subnet":"172.31.1.0/24",` +
		`"allow":[],"deny":["10.0.0.0/8"]}`
	if err := os.WriteFile(PolicyPath(dir), []byte(inForce), 0o600); err != nil {
		t.Fatal(err)
	}
	noKernel := func(egress.Policy) error { return nil }

	// build runs between the read of the policy in force and the write
	// of its successor, which is the whole of the section the lock has
	// to hold shut.
	var inside, peak atomic.Int32
	install := func() error {
		return installPolicyWith(dir, func(current egress.Policy) (egress.Policy, error) {
			n := inside.Add(1)
			for {
				seen := peak.Load()
				if n <= seen || peak.CompareAndSwap(seen, n) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			inside.Add(-1)
			next := current
			next.Deny = []string{"10.0.0.0/8", "192.168.0.0/16"}
			return next, next.Validate()
		}, noKernel)
	}

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := install(); err != nil {
				t.Errorf("install: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := peak.Load(); got != 1 {
		t.Errorf("%d installers held the critical section at once; want 1 — "+
			"each one past the read is building its successor from a policy "+
			"another is about to replace", got)
	}
}

// TestAReaderCrossingAnInstallDoesNotOverCountGenerations: the policy's
// generation counts installs, not readers that raced one.
//
// The generation is what makes the relay retire its pooled transport and
// every idle connection in it — a pooled request never reaches the
// dialer, so a policy that moved has no other way to reach it. Advancing
// it for a file that did not change is that pool thrown away for
// nothing, on a relay that may be carrying a job's whole traffic.
//
// The interleaving that causes it needs the file to change between one
// reader's stat and its turn at the lock, which no test can schedule.
// What this does instead is race enough readers against enough installs
// that the window is entered, and assert the invariant that survives any
// interleaving: the generation never exceeds the number of distinct
// policies actually installed.
func TestAReaderCrossingAnInstallDoesNotOverCountGenerations(t *testing.T) {
	dir := t.TempDir()
	path := PolicyPath(dir)
	policy := func(deny string) []byte {
		return []byte(`{"internal_subnet":"172.31.0.0/24","uplink_subnet":"172.31.1.0/24",` +
			`"allow":[],"deny":["` + deny + `"]}`)
	}
	if err := atomicfile.Replace(path, policy("10.0.0.0/8"), 0o600, -1, -1); err != nil {
		t.Fatal(err)
	}
	store := &PolicyStore{Path: path}
	if _, err := store.Current(); err != nil {
		t.Fatal(err)
	}

	const installs = 40
	denies := []string{"10.0.0.0/8", "192.168.0.0/16", "172.16.0.0/12"}

	stop := make(chan struct{})
	var readers sync.WaitGroup
	for range 8 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := store.Current(); err != nil {
					// A reader that returned quietly would leave the
					// ceiling below satisfied by eight dead goroutines.
					t.Errorf("reader: %v", err)
					return
				}
			}
		}()
	}
	// Deferred as well as called, so a failure out of the loop below
	// cannot leave eight goroutines spinning for the rest of the run.
	settle := sync.OnceFunc(func() {
		close(stop)
		readers.Wait()
	})
	defer settle()

	for i := range installs {
		if err := atomicfile.Replace(path, policy(denies[i%len(denies)]), 0o600, -1, -1); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	settle()

	// One for the first read, and at most one per install. Two installs
	// inside the same nanosecond-and-size stamp are indistinguishable to
	// the store, so this is an upper bound rather than an equality.
	if got, max := store.Generation(), uint64(installs+1); got > max {
		t.Errorf("generation = %d after %d installs; want at most %d. "+
			"The extra reloads are pooled transports retired for a policy that did not move",
			got, installs, max)
	}
}

// TestAnEqualLengthInstallIsNotMissed: what counts as a policy change is
// the document's bytes, not its modification time and size.
//
// Linux stamps mtime from the coarse clock, which advances once per tick,
// so two documents of equal length installed inside one tick carry the
// same pair. Deciding on that pair, the second install never reaches the
// relay at all: the decider stays as it was, the generation does not
// move, and the pooled transport is not retired either — a tightening
// that silently does not take effect.
func TestAnEqualLengthInstallIsNotMissed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.json")
	doc := func(deny string) string {
		return `{"internal_subnet":"172.31.0.0/24","uplink_subnet":"172.31.1.0/24",` +
			`"allow":[],"deny":["` + deny + `"]}`
	}
	first, second := doc("10.20.20.0/24"), doc("10.30.30.0/24")
	if len(first) != len(second) {
		t.Fatalf("the two documents differ in length (%d vs %d); size alone would "+
			"separate them and the test would prove nothing", len(first), len(second))
	}

	// One instant for both installs: the tick they would have shared.
	tick := time.Unix(1_700_000_000, 0)
	install := func(body string) {
		t.Helper()
		if err := atomicfile.Replace(path, []byte(body), 0o600, -1, -1); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, tick, tick); err != nil {
			t.Fatal(err)
		}
	}

	s := &PolicyStore{Path: path}
	install(first)
	decider, err := s.Current()
	if err != nil {
		t.Fatal(err)
	}
	if decider.Allowed(netip.MustParseAddr("10.20.20.5")) {
		t.Fatal("the first policy did not take effect; nothing after this means anything")
	}
	baseline := s.Generation()

	install(second)
	decider, err = s.Current()
	if err != nil {
		t.Fatal(err)
	}
	if !decider.Allowed(netip.MustParseAddr("10.20.20.5")) {
		t.Error("the second install never reached the relay; the range it stopped denying is still refused")
	}
	if decider.Allowed(netip.MustParseAddr("10.30.30.5")) {
		t.Error("the second install never reached the relay; the range it began denying is still permitted")
	}
	if s.Generation() == baseline {
		t.Error("the generation did not move on the second install; the pooled transport " +
			"keeps every connection the previous policy authorised")
	}
}
