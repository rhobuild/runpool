package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rhobuild/runpool/internal/allocator"
	"github.com/rhobuild/runpool/internal/assignment"
	"github.com/rhobuild/runpool/internal/config"
	"github.com/rhobuild/runpool/internal/platform/githubactions"
	"github.com/rhobuild/runpool/internal/store"
)

// flakyScaleSets fails a fixed number of times before it succeeds, which
// is what an unreachable provider looks like from the loop.
type flakyScaleSets struct {
	nullProvider
	failures int32
	calls    atomic.Int32
}

func (f *flakyScaleSets) EnsureScaleSet(context.Context, string, string, int, bool, func() error) (githubactions.ScaleSet, error) {
	if f.calls.Add(1) <= f.failures {
		return githubactions.ScaleSet{}, errors.New("provider unreachable")
	}
	return githubactions.ScaleSet{ID: 77}, nil
}

// A provider that cannot be reached costs the binding its turn, not the
// process its startup: the loop retries the scale set until it exists,
// which is what lets startup reconciliation run before any provider call.
func TestAnUnreachableProviderDoesNotEndTheLoop(t *testing.T) {
	h := newHarness(t, 1)
	h.bind.ref = config.TargetRef{
		Scope:        config.ScopeRepository,
		Owner:        "acme",
		Repository:   "app",
		CanonicalURL: "https://github.com/acme/app",
	}
	h.bind.ensured = false
	h.bind.scaleSetID = 0
	sets := &flakyScaleSets{failures: 2}
	h.bind.gh = sets
	h.srv.pollBackoff = time.Millisecond
	// The broker stays unreachable too, so the loop is exercised in the
	// state this test is about: connected to nothing, still running.
	// Reaching the session open is the loop's own statement that the
	// scale set is ready: it is the step after ensure. Waiting for the
	// second attempt rather than the first is what makes the failure of
	// the first one already recorded by the time this reads the store.
	var once sync.Once
	var attempts atomic.Int32
	failing := make(chan struct{})
	h.bind.newSession = func(context.Context) (providerSession, error) {
		if attempts.Add(1) >= 2 {
			once.Do(func() { close(failing) })
		}
		return nil, errors.New("broker unreachable")
	}

	ctx, cancel := context.WithCancel(t.Context())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		h.srv.loop(ctx, h.bind)
	}()

	select {
	case <-failing:
	case <-time.After(10 * time.Second):
		cancel()
		<-stopped
		t.Fatalf("the loop gave up after %d attempts", sets.calls.Load())
	}
	cancel()
	<-stopped

	if got := sets.calls.Load(); got != 3 {
		t.Errorf("ensure attempts = %d, want 3 (two failures then the success)", got)
	}
	if !h.bind.ensured {
		t.Error("the binding is not marked ensured after the provider answered")
	}
	if h.bind.scaleSetID != 77 {
		t.Errorf("scaleSetID = %d, want the id the provider returned", h.bind.scaleSetID)
	}

	// The same run is what a reporting command reads: the scale set was
	// reached, and the broker behind it is what is failing now.
	var contact store.ProviderContact
	if err := h.srv.store.Tx(t.Context(), func(tx *store.Tx) error {
		bindings, err := tx.Bindings()
		for _, b := range bindings {
			if b.ID == h.bind.bindingID {
				contact = b.Contact
			}
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if contact.LastContact.IsZero() {
		t.Error("the successful ensure recorded no contact")
	}
	if contact.LastError == "" {
		t.Error("the unreachable broker recorded no failure")
	}
	if contact.LastContact.After(contact.LastErrorAt) {
		t.Errorf("failure at %v predates contact at %v; the binding would read as healthy",
			contact.LastErrorAt, contact.LastContact)
	}
}

// TestAnUnreachableProviderDoesNotSpendTheQueue is the regression test
// for a startup outage consuming every queued attempt's retry budget.
//
// Draining a ready attempt mints a JIT credential against this binding's
// scale set, so it is not local work and cannot run before the set is
// confirmed. When it did, a restart that met an unreachable provider
// leased each queued attempt against an unconfirmed id, failed, and spent
// one of its three servings per pass — holding the whole queue for a
// person within seconds, for an outage that would have passed.
func TestAnUnreachableProviderDoesNotSpendTheQueue(t *testing.T) {
	h := newHarness(t, 4)
	h.bind.ref = config.TargetRef{
		Scope: config.ScopeRepository, Owner: "acme", Repository: "app",
		CanonicalURL: "https://github.com/acme/app",
	}
	if err := h.deliver(
		assignment.WorkloadAssignment{SourceWorkloadKey: "job-1"},
		assignment.WorkloadAssignment{SourceWorkloadKey: "job-2"},
	); err != nil {
		t.Fatal(err)
	}
	if got := len(h.ready()); got != 2 {
		t.Fatalf("queued %d attempts, want 2", got)
	}

	// The provider is unreachable for the whole run, which is the outage
	// this is about.
	var attempts atomic.Int32
	tried := make(chan struct{})
	var once sync.Once
	h.bind.ensured = false
	h.bind.gh = &refusingScaleSets{calls: &attempts, reached: func() {
		once.Do(func() { close(tried) })
	}}
	h.srv.pollBackoff = time.Millisecond
	h.bind.newSession = func(context.Context) (providerSession, error) {
		return nil, errors.New("broker unreachable")
	}

	ctx, cancel := context.WithCancel(t.Context())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		h.srv.loop(ctx, h.bind)
	}()
	<-tried
	// Wait for the second pass rather than sleeping a window and hoping
	// one fits. The backoff is a millisecond, but a loop under the race
	// detector and coverage instrumentation on a loaded machine can take
	// far longer than that, and this asked for two passes inside fifty —
	// so the failure it reported was the machine's, not the loop's. A
	// window wide enough for the slowest machine would be dead time on
	// every other run; waiting for the thing itself is neither.
	retried := time.After(10 * time.Second)
	for attempts.Load() < 2 {
		select {
		case <-retried:
			cancel()
			<-stopped
			t.Fatalf("the loop tried the provider %d time(s) in ten seconds; "+
				"the outage has to keep it retrying", attempts.Load())
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	<-stopped
	if got := len(h.ready()); got != 2 {
		t.Errorf("%d attempts still queued, want 2: the outage spent them", got)
	}
	if launched := h.launchedAttempts(); len(launched) != 0 {
		t.Errorf("launched %v against an unconfirmed scale set", launched)
	}
}

type refusingScaleSets struct {
	nullProvider
	calls   *atomic.Int32
	reached func()
}

func (r *refusingScaleSets) EnsureScaleSet(context.Context, string, string, int, bool, func() error) (githubactions.ScaleSet, error) {
	r.calls.Add(1)
	r.reached()
	return githubactions.ScaleSet{}, errors.New("provider unreachable")
}

// TestProviderReachIsWrittenOnTransition: the record answers what is
// wrong and how long it has been wrong. Rewriting the same sentence every
// pass answers neither, and each write takes the store's only connection
// ahead of the lease transitions that need it.
func TestProviderReachIsWrittenOnTransition(t *testing.T) {
	h := newHarness(t, 1)
	h.bind.ref = config.TargetRef{
		Scope: config.ScopeRepository, Owner: "acme", Repository: "app",
		CanonicalURL: "https://github.com/acme/app",
	}
	ctx := t.Context()
	read := func() store.ProviderContact {
		t.Helper()
		var c store.ProviderContact
		if err := h.srv.store.Tx(ctx, func(tx *store.Tx) error {
			bindings, err := tx.Bindings()
			for _, b := range bindings {
				if b.ID == h.bind.bindingID {
					c = b.Contact
				}
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return c
	}

	same := errors.New("provider unreachable")
	h.srv.recordProviderFailure(ctx, h.bind, same)
	first := read()
	if first.LastError != same.Error() {
		t.Fatalf("first failure not recorded: %+v", first)
	}

	// The same failure again: nothing changed, so the recorded moment must
	// not move — it is what dates the outage. The pause is what makes
	// "did not write" observable at all, since the only evidence of a
	// write is a moment that moved.
	time.Sleep(5 * time.Millisecond)
	h.srv.recordProviderFailure(ctx, h.bind, same)
	if again := read(); !again.LastErrorAt.Equal(first.LastErrorAt) {
		t.Errorf("a repeated failure moved its own start from %v to %v",
			first.LastErrorAt, again.LastErrorAt)
	}

	// A different failure is a transition and is written at once.
	time.Sleep(5 * time.Millisecond)
	h.srv.recordProviderFailure(ctx, h.bind, errors.New("401 Unauthorized"))
	if got := read(); got.LastError != "401 Unauthorized" {
		t.Errorf("a changed failure was not recorded: %+v", got)
	}

	// Recovery is a transition too, and must not wait out the heartbeat.
	h.srv.recordProviderContact(ctx, h.bind)
	got := read()
	if got.LastError != "" {
		t.Errorf("recovery left the failure in place: %+v", got)
	}
	if got.LastContact.IsZero() {
		t.Error("recovery recorded no contact")
	}
}

// TestACrashBetweenCreatingAndRecordingConverges: creating a scale set at
// the provider and writing down its id are two steps. A process that dies
// between them leaves a set the provider has and this instance cannot
// account for, and a set that merely shares a name is a stranger's — so
// every later start correctly refused to adopt it and the binding never
// served again.
//
// The intention is what tells the two apart, and it is only honoured when
// it was actually recorded.
func TestACrashBetweenCreatingAndRecordingConverges(t *testing.T) {
	for name, tc := range map[string]struct {
		seedIntent   bool
		wantIntended bool
	}{
		"a binding that never asked for this name":  {false, false},
		"a binding killed after it created the set": {true, true},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, 1)
			h.bind.ref = config.TargetRef{
				Scope: config.ScopeRepository, Owner: "acme", Repository: "app",
				CanonicalURL: "https://github.com/acme/app",
			}
			h.bind.ensured = false
			h.bind.scaleSetID = 0

			if tc.seedIntent {
				// Exactly what the first pass leaves behind when it dies
				// between the provider call and the record: the name
				// written down, and no id.
				h.inStore(func(tx *store.Tx) error {
					return tx.RecordGitHubBindingMetadata(h.bind.bindingID,
						string(h.bind.ref.Scope), h.bind.ref.CanonicalURL,
						h.bind.target.RunnerGroup, h.bind.scaleSetName, 0)
				})
			}

			// The set exists at the provider from the moment the call
			// returns, so the name has to be written down before it: that
			// is the whole of what makes the crash recoverable.
			var namedBeforeTheCall bool
			sets := &recordingScaleSets{id: 42, duringCall: func() {
				h.inStore(func(tx *store.Tx) error {
					_, recorded, err := tx.GitHubScaleSet(h.bind.bindingID)
					namedBeforeTheCall = recorded
					return err
				})
			}}
			h.bind.gh = sets
			if err := h.srv.ensureScaleSet(t.Context(), h.bind); err != nil {
				t.Fatalf("ensureScaleSet: %v", err)
			}
			if sets.lastIntended != tc.wantIntended {
				t.Errorf("intended = %v, want %v: a real provider decides whether to adopt on this",
					sets.lastIntended, tc.wantIntended)
			}
			if !namedBeforeTheCall {
				t.Error("the callback the controller hands the provider did not write the name down; a crash after the create would leave a set nothing can account for")
			}
			if h.bind.scaleSetID != 42 || !h.bind.ensured {
				t.Errorf("binding did not converge: id=%d ensured=%v", h.bind.scaleSetID, h.bind.ensured)
			}
			// And the id is recorded, so the next start needs no
			// intention at all.
			var recorded int64
			h.inStore(func(tx *store.Tx) error {
				var err error
				recorded, err = tx.GitHubScaleSetID(h.bind.bindingID)
				return err
			})
			if recorded != 42 {
				t.Errorf("recorded id = %d, want 42", recorded)
			}
		})
	}
}

type recordingScaleSets struct {
	nullProvider
	id           int
	lastIntended bool
	// duringCall runs while the provider call is in flight, which is the
	// only moment that can show what was written down before it.
	duringCall func()
}

func (r *recordingScaleSets) EnsureScaleSet(_ context.Context, _, _ string, _ int, intended bool, recordIntent func() error) (githubactions.ScaleSet, error) {
	r.lastIntended = intended
	// The name is free on this path, so the real adapter records the
	// intention here, immediately before it creates the set.
	if err := recordIntent(); err != nil {
		return githubactions.ScaleSet{}, err
	}
	if r.duringCall != nil {
		r.duringCall()
	}
	return githubactions.ScaleSet{ID: r.id}, nil
}

// TestStartupSaysWhereEachCredentialTravels: any https host is accepted
// by design, so the boundary has to be visible instead of enforced. A
// host GitHub operates logs as ordinary; any other logs at Warn naming
// the target, the host and the credential — which is what makes a
// one-letter typosquat visible on the first start instead of quietly
// authenticated forever.
func TestStartupSaysWhereEachCredentialTravels(t *testing.T) {
	build := func(t *testing.T, url string) string {
		t.Helper()
		st, err := store.Open(t.TempDir(), store.DefaultRetryBudget)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		var buf strings.Builder
		s := &Controller{
			log:       slog.New(slog.NewTextHandler(&buf, nil)),
			store:     st,
			alloc:     allocator.New(),
			byBinding: map[assignment.BindingID]*binding{},
		}
		cfg := &config.Config{
			Instance:    config.Instance{Name: "test"},
			Credentials: []config.Credential{{ID: "gh", TokenEnv: "TOKEN"}},
			Targets: []config.Target{{
				ID: "app", URL: url, CredentialID: "gh",
				Tiers: []config.TierBinding{{TierID: "standard", ScaleSetName: "runpool-standard"}},
			}},
			Tiers: []config.Tier{{ID: "standard", Parallelism: 1}},
		}
		environ := func(k string) string {
			if k == "TOKEN" {
				return "t0ken"
			}
			return ""
		}
		if err := s.buildBindings(t.Context(), cfg, environ); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}

	logged := build(t, "https://github.com/acme/app")
	if !strings.Contains(logged, "host=github.com") {
		t.Errorf("a hosted target's log does not name the host:\n%s", logged)
	}
	if strings.Contains(logged, "level=WARN") {
		t.Errorf("a hosted target warned:\n%s", logged)
	}

	logged = build(t, "https://ghes.internal/acme/app")
	if !strings.Contains(logged, "level=WARN") ||
		!strings.Contains(logged, "host=ghes.internal") ||
		!strings.Contains(logged, "credential=gh") {
		t.Errorf("a non-hosted target must warn naming host and credential:\n%s", logged)
	}
}

// TestTheDurableBindingKeyDoesNotMoveWithTheURLParser: a binding's
// durable identity is what an operator configured, not a parsed form of
// the address they wrote.
//
// The key was built from the parsed scope and the canonical URL, so a
// change to how a URL is read moves the key of a deployment that changed
// nothing: the next start inserts a second binding beside the first, and
// the original is left in `status` for good with no command that removes
// it. Every delivery, attempt and lease hangs off that row, so the
// history goes with it.
// TestTheBindingKeyIsVersionedAndPinned: the durable binding key is a
// literal here, not a comparison against itself.
//
// Its sibling in internal/assignment pins the delivery key this way, and
// that is the cheaper of the two to move: a re-keyed delivery is
// processed again and finds its attempts by workload key. This one is
// the expensive one. Moving it renames every binding — the old row is
// forgotten on the next startup with the scale set id recorded against
// it, and each binding then meets a provider that already holds its
// scale set and refuses to adopt one it has no record of creating.
//
// The test that existed compared sourceBindingKey's output to
// sourceBindingKey's output, so any change to the encoding, version
// included, left the suite green. The costly one was the unpinned one.
func TestTheBindingKeyIsVersionedAndPinned(t *testing.T) {
	got := sourceBindingKey(config.Target{ID: "app", RunnerGroup: "default"}, "runpool-standard")
	if want := assignment.SourceBindingKey("v2|app|default|runpool-standard"); got != want {
		t.Errorf("sourceBindingKey = %q; want %q. This value keys every delivery, attempt "+
			"and lease a binding owns, and moving it is a migration rather than an encoding change",
			got, want)
	}
}

func TestTheDurableBindingKeyDoesNotMoveWithTheURLParser(t *testing.T) {
	base := config.Target{ID: "app", RunnerGroup: "default"}
	want := sourceBindingKey(base, "runpool-standard")

	for name, url := range map[string]string{
		"a plain repository URL": "https://github.com/acme/app",
		"the clone form":         "https://github.com/acme/app.git",
		"a trailing slash":       "https://github.com/acme/app/",
		"a different host":       "https://ghe.example.com/acme/app",
	} {
		t.Run(name, func(t *testing.T) {
			target := base
			target.URL = url
			if got := sourceBindingKey(target, "runpool-standard"); got != want {
				t.Errorf("key = %q for %s; want %q — the identity moved with the address form",
					got, url, want)
			}
		})
	}

	// And it still separates what genuinely differs.
	other := base
	other.RunnerGroup = "isolated"
	if sourceBindingKey(other, "runpool-standard") == want {
		t.Error("two runner groups share one binding key")
	}
	if sourceBindingKey(base, "runpool-large") == want {
		t.Error("two scale sets share one binding key")
	}
}

// occupiedScaleSets answers the way the provider does when the name is
// already taken: it adopts only for a caller that can show it created the
// set, which is what the intention claims.
type occupiedScaleSets struct {
	nullProvider
	adopted int
}

func (o *occupiedScaleSets) EnsureScaleSet(_ context.Context, _, name string, knownID int, intended bool, _ func() error) (githubactions.ScaleSet, error) {
	if knownID == 0 && !intended {
		return githubactions.ScaleSet{}, fmt.Errorf(
			"scale set %q already exists and this instance has no record of creating it", name)
	}
	o.adopted++
	return githubactions.ScaleSet{ID: 99, Adopted: true}, nil
}

// TestARefusedNameStaysRefusedOnTheNextPass: a set that merely shares a
// name is a stranger's, and the refusal has to survive the retry that
// follows it. The intention exists to tell a set this instance created
// and failed to write down from one that was simply already there — so an
// intention written by a pass that was then refused answers that question
// with its own failure, and the binding adopts the stranger one poll
// later.
func TestARefusedNameStaysRefusedOnTheNextPass(t *testing.T) {
	h := newHarness(t, 1)
	h.bind.ref = config.TargetRef{
		Scope: config.ScopeRepository, Owner: "acme", Repository: "app",
		CanonicalURL: "https://github.com/acme/app",
	}
	h.bind.ensured = false
	h.bind.scaleSetID = 0
	sets := &occupiedScaleSets{}
	h.bind.gh = sets

	if err := h.srv.ensureScaleSet(t.Context(), h.bind); err == nil {
		t.Fatal("the first pass adopted a scale set this instance did not create")
	}
	if err := h.srv.ensureScaleSet(t.Context(), h.bind); err == nil {
		t.Fatal("the second pass adopted it: the refusal was defeated by the intention the refused pass wrote")
	}
	if sets.adopted != 0 {
		t.Errorf("a stranger's scale set was adopted %d times", sets.adopted)
	}
	if h.bind.ensured {
		t.Error("the binding reports itself ensured against a scale set it does not own")
	}
}
