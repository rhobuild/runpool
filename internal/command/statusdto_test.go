package command

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/rhobuild/runpool/internal/cache"
	"github.com/rhobuild/runpool/internal/capsule"
	"github.com/rhobuild/runpool/internal/config"
	"github.com/rhobuild/runpool/internal/platform/docker"
	"github.com/rhobuild/runpool/internal/store"
)

// TestStatusDocumentShape pins the reporting contract: versioned,
// snake_case, and every collection an array even when empty — a
// consumer branches on length, and a null would make it branch on
// presence instead.
func TestStatusDocumentShape(t *testing.T) {
	doc := statusDocument(store.Snapshot{InstanceID: "i-1", SchemaVersion: 1}, &config.Config{
		Host:  config.Host{Topology: config.HostTopologySharedDaemon},
		Tiers: []config.Tier{{ID: "standard", Parallelism: 1}},
	}, nil, daemonObservation{}, "runpool-capsule:dev")
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)

	if !strings.Contains(body, `"api_version":"v1"`) {
		t.Errorf("document is not versioned: %s", body)
	}
	for _, field := range []string{
		`"bindings":[]`, `"leases":[]`, `"cache_lanes":[]`,
		`"manual_review":[]`, `"containers":[]`, `"networks":[]`,
		`"volumes":[]`, `"discrepancies":[]`,
	} {
		if !strings.Contains(body, field) {
			t.Errorf("empty collection did not serialize as []: want %s in %s", field, body)
		}
	}
	if strings.Contains(body, "null,") || strings.HasSuffix(body, "null}") {
		// disk_pressure is the one legitimate null: an absent
		// measurement is absent, not empty.
		if !strings.Contains(body, `"disk_pressure":null`) {
			t.Errorf("unexpected null in %s", body)
		}
	}
	for _, upper := range []string{"InstanceID", "SourceBindingKey", "LeaseID", "AttemptID"} {
		if strings.Contains(body, upper) {
			t.Errorf("persistence field name %q leaked into the reporting document", upper)
		}
	}
}

func TestConfiguredStatusConfigFromEnvironment(t *testing.T) {
	environ := func(name string) string {
		if name == "RUNPOOL_HOST_TOPOLOGY" {
			return "shared-daemon"
		}
		return ""
	}
	got := configuredStatusConfig(environ)
	if got == nil || got.Host.Topology != "shared-daemon" || got.Tiers[0].Parallelism != 1 {
		t.Fatalf("status config = %+v; want shared-daemon with parallelism one", got)
	}
}

// TestStatusDiscrepanciesCoverEveryKind: agreement judged from
// containers alone once hid every leaked network and volume; the
// comparison must name all three, and must not run at all when the
// daemon could not be asked.
func TestStatusDiscrepanciesCoverEveryKind(t *testing.T) {
	leases := []store.Lease{
		{ID: "lease-live", State: store.LeaseWorkloadRunning},
		{ID: "lease-done", State: store.LeaseReleased},
	}
	obs := daemonObservation{
		containers: []docker.OwnedContainer{
			{Name: "c-orphan", LeaseID: "lease-done"},
			// The instance's own helpers carry no lease. One in flight is
			// the daemon and the books agreeing; one left stopped is a
			// leak, and telling them apart is the whole report.
			{Name: "probe-running", Role: "probe", Running: true},
			{Name: "probe-leaked", Role: "probe"},
		},
		networks: []docker.OwnedResource{
			{ID: "net-orphan", LeaseID: "lease-done"},
			{ID: "uplink", Role: capsule.RoleUplink},
		},
		volumes: []docker.OwnedResource{
			{ID: "vol-unclassified"},
			{ID: "lane", Role: cache.RoleCacheLane},
		},
	}
	got := discrepancies(leases, obs)

	for _, want := range []string{
		"container c-orphan belongs to no live lease",
		"container probe-leaked belongs to no live lease",
		"lease lease-live claims to be running with no container",
		"network net-orphan belongs to no live lease",
		"volume vol-unclassified carries no lease and no persistent role",
	} {
		found := false
		for _, d := range got {
			if d == want {
				found = true
			}
		}
		if !found {
			t.Errorf("missing discrepancy %q in %v", want, got)
		}
	}
	for _, d := range got {
		if strings.Contains(d, "uplink") || strings.Contains(d, "lane") {
			t.Errorf("instance infrastructure reported as a discrepancy: %s", d)
		}
		if strings.Contains(d, "probe-running") {
			t.Errorf("a helper still measuring reported as a discrepancy: %s", d)
		}
	}

	// An unreachable daemon yields no comparison, and the document says
	// why instead of claiming agreement.
	doc := statusDocument(store.Snapshot{InstanceID: "i"}, nil, nil, daemonObservation{err: errors.New("daemon down")}, "")
	if doc.Discrepancies != nil {
		t.Errorf("discrepancies = %v with an unreachable daemon; want null (not compared)", doc.Discrepancies)
	}
	if doc.DockerError == "" {
		t.Error("an unreachable daemon must be reported in docker_error")
	}
}

func TestSchedulingStatusCountsEveryUnreleasedLease(t *testing.T) {
	one := 1
	cfg := &config.Config{
		Scheduling: config.Scheduling{Parallelism: &one},
		Tiers: []config.Tier{
			{ID: "small", Parallelism: 1},
			{ID: "large", Parallelism: 1},
		},
	}
	leases := []store.Lease{
		{ID: "active", TierID: "large", State: store.LeaseCleaning},
		{ID: "released", TierID: "small", State: store.LeaseReleased},
	}
	got := schedulingStatus(cfg, leases, map[int64]int{7: 3}, "")
	if got.Mode != "global" || got.Active != 1 || got.Available != 0 || got.EffectiveParallelism != 1 {
		t.Fatalf("scheduling = %+v", got)
	}
	if got.Tiers[1].Active != 1 || got.Tiers[1].Available != 0 {
		t.Fatalf("large tier accounting = %+v", got.Tiers[1])
	}
}

// TestReleasedTotalIsTheStoresCountNotTheArrayLength. The leases array
// carries only the most recently finished, so a consumer measuring it
// understates the history by every job past the bound. The figure has to
// come from the store, which is the reason it is published at all.
func TestReleasedTotalIsTheStoresCountNotTheArrayLength(t *testing.T) {
	doc := statusDocument(store.Snapshot{
		InstanceID: "i-1",
		Leases: []store.Lease{
			{ID: "a", State: store.LeaseReleased},
			{ID: "b", State: store.LeaseReleased},
		},
		ReleasedTotal: 4321,
	}, nil, nil, daemonObservation{}, "")

	if doc.ReleasedTotal != 4321 {
		t.Errorf("released_total = %d; want the store's count, 4321", doc.ReleasedTotal)
	}
	if doc.ReleasedTotal == len(doc.Leases) {
		t.Errorf("released_total was taken from the array, which is bounded by design")
	}
}

// TestQueuedWorkIsReported. Active counts leases, and an attempt waiting
// for admission has none — so an instance holding a backlog and running
// one job reported exactly like an idle one. A queue that stops draining
// is the shape a stuck binding takes, and it was invisible.
func TestQueuedWorkIsReported(t *testing.T) {
	cfg := &config.Config{Tiers: []config.Tier{{ID: "standard", Parallelism: 2}}}
	got := schedulingStatus(cfg, nil, map[int64]int{1: 4, 2: 2}, "")
	if got.Queued != 6 {
		t.Errorf("queued = %d; want the sum across bindings, 6", got.Queued)
	}
	if idle := schedulingStatus(cfg, nil, nil, ""); idle.Queued != 0 {
		t.Errorf("an instance with nothing waiting reports queued = %d", idle.Queued)
	}
}

// TestTierReportsTheImageItRuns: a deployment that replaced the capsule
// for a tier is outside the configuration the release gates observed, and
// the report says so rather than leaving it to be inferred from a
// configuration file the reader may not have.
func TestTierReportsTheImageItRuns(t *testing.T) {
	const shipped = "ghcr.io/rhobuild/runpool/capsule@sha256:" +
		"4444444444444444444444444444444444444444444444444444444444444444"
	const operators = "ghcr.io/acme/capsule@sha256:" +
		"5555555555555555555555555555555555555555555555555555555555555555"
	cfg := &config.Config{Tiers: []config.Tier{
		{ID: "standard", Parallelism: 1},
		{ID: "heavy", Parallelism: 1, CapsuleImage: operators},
	}}

	got := schedulingStatus(cfg, nil, nil, shipped)
	if len(got.Tiers) != 2 {
		t.Fatalf("tiers = %+v", got.Tiers)
	}
	if got.Tiers[0].CapsuleImage != shipped {
		t.Errorf("a tier naming no image reports %q, want the shipped capsule", got.Tiers[0].CapsuleImage)
	}
	if got.Tiers[1].CapsuleImage != operators {
		t.Errorf("a tier naming an image reports %q, want its own", got.Tiers[1].CapsuleImage)
	}
}

// TestStatusReportsWithoutAResolvableCapsuleImage: a capsule image this
// command cannot resolve is a finding to report, not a reason to answer
// nothing.
//
// Resolving it in the command wiring meant one unset or conflicting
// environment variable returned an error and printed no document at all
// — taking the daemon comparison, the lease list and every other fact
// down with it, and answering a --json caller with prose on stderr and a
// non-zero exit. That is the same parse failure the pre-serve form is
// careful to avoid.
func TestStatusReportsWithoutAResolvableCapsuleImage(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RUNPOOL_STATE_DIR", dir)
	st, err := store.Open(dir, store.DefaultRetryBudget)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()

	// A release build refuses an override that disagrees with it, which
	// is the shape an operator meets after setting the variable by hand.
	const release = "ghcr.io/rhobuild/runpool/capsule@sha256:" +
		"1111111111111111111111111111111111111111111111111111111111111111"
	t.Setenv("RUNPOOL_CAPSULE_IMAGE", "ghcr.io/example/other:latest")

	var out, errOut bytes.Buffer
	if err := runStatus(IO{Out: &out, Err: &errOut}, true, release); err != nil {
		t.Fatalf("status refused to answer: %v", err)
	}

	var doc statusDoc
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("the document does not decode: %v\n%s", err, out.String())
	}
	if !doc.Served {
		t.Fatal("the served document was not produced")
	}
	if doc.CapsuleImageError == "" {
		t.Error("the document reports no capsule image error; a reader takes the tier images " +
			"for what a launch would run")
	}
}

// TestBothStatusAnswersShareOneEnvelope: the two forms of a v1 status
// document are the same document.
//
// The pre-serve form used to be a hand-built map that re-spelled every
// tag, so a rename on the struct could leave the two disagreeing with
// nothing to notice — and a consumer branching on `served`, which the
// document tells it to do, would then meet fields it had no name for.
func TestBothStatusAnswersShareOneEnvelope(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RUNPOOL_STATE_DIR", dir)

	decode := func(t *testing.T, raw []byte) statusDoc {
		t.Helper()
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		var doc statusDoc
		if err := dec.Decode(&doc); err != nil {
			t.Fatalf("the document carries a field statusDoc does not name: %v\n%s", err, raw)
		}
		return doc
	}

	// Pre-serve: no state directory has been written yet.
	var out bytes.Buffer
	if err := runStatus(IO{Out: &out, Err: &bytes.Buffer{}}, true, ""); err != nil {
		t.Fatal(err)
	}
	pre := decode(t, out.Bytes())
	if pre.Served {
		t.Error("the pre-serve form claims to be served")
	}
	if pre.StateDir != dir || pre.Detail == "" {
		t.Errorf("the pre-serve form lost its payload: state_dir %q, detail %q", pre.StateDir, pre.Detail)
	}

	// Served: the same envelope, the other branch.
	st, err := store.Open(dir, store.DefaultRetryBudget)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	out.Reset()
	if err := runStatus(IO{Out: &out, Err: &bytes.Buffer{}}, true, ""); err != nil {
		t.Fatal(err)
	}
	if served := decode(t, out.Bytes()); !served.Served {
		t.Error("the served form does not say so")
	}
}
