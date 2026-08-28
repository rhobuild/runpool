package githubactions

import (
	"errors"
	"slices"
	"testing"

	"github.com/rhobuild/runpool/internal/assignment"
)

func offer(key assignment.SourceWorkloadKey, request int64) assignment.WorkloadAssignment {
	return assignment.WorkloadAssignment{
		SourceWorkloadKey: string(key), TenantKey: "acme", ProjectKey: "app",
		SourceRequestID: request,
	}
}

// TestOfferableRefusesToGuessBetweenSharedIDs. The request id is the only
// handle AcquireJobs speaks, and the domain type is explicit that it is
// never an identity. So two availables carrying the same id make a grant
// for that id ambiguous, and the wire says nothing more: claiming the
// first would run a job that was not granted while losing the one that
// was. Refusing to offer either leaves both queued upstream, which is
// what a declined offer already looks like.
func TestOfferableRefusesToGuessBetweenSharedIDs(t *testing.T) {
	for name, tc := range map[string]struct {
		available []assignment.WorkloadAssignment
		wantIDs   []int64
		wantKeys  []string
	}{
		"distinct ids are all offered": {
			available: []assignment.WorkloadAssignment{offer("a", 7), offer("b", 9)},
			wantIDs:   []int64{7, 9},
			wantKeys:  []string{"a", "b"},
		},
		"the broker's order is kept": {
			available: []assignment.WorkloadAssignment{offer("a", 9), offer("b", 7)},
			wantIDs:   []int64{9, 7},
			wantKeys:  []string{"a", "b"},
		},
		"a shared id takes both out": {
			available: []assignment.WorkloadAssignment{offer("a", 7), offer("b", 7), offer("c", 9)},
			wantIDs:   []int64{9},
			wantKeys:  []string{"c"},
		},
		"zero is an id like any other": {
			available: []assignment.WorkloadAssignment{offer("a", 0)},
			wantIDs:   []int64{0},
			wantKeys:  []string{"a"},
		},
		"and zero shared is still ambiguous": {
			available: []assignment.WorkloadAssignment{offer("a", 0), offer("b", 0)},
			wantIDs:   []int64{},
			wantKeys:  []string{},
		},
		"nothing offered": {
			available: nil,
			wantIDs:   []int64{},
			wantKeys:  []string{},
		},
	} {
		ids, byID := offerable(tc.available)
		if !slices.Equal(ids, tc.wantIDs) {
			t.Errorf("%s: offered %v; want %v", name, ids, tc.wantIDs)
		}
		var keys []string
		for _, id := range ids {
			keys = append(keys, byID[id].SourceWorkloadKey)
		}
		if !slices.Equal(keys, tc.wantKeys) && !(len(keys) == 0 && len(tc.wantKeys) == 0) {
			t.Errorf("%s: paired %v; want %v", name, keys, tc.wantKeys)
		}
		if len(byID) != len(ids) {
			t.Errorf("%s: %d ids offered but %d pairings; every offer must name one workload",
				name, len(ids), len(byID))
		}
	}
}

// TestOfferablePairingIsSpentOnce. A grant is consumed by the workload it
// names, so a broker that answers with the same id twice claims one
// workload and strands the repeat rather than running the job twice.
func TestOfferablePairingIsSpentOnce(t *testing.T) {
	ids, byID := offerable([]assignment.WorkloadAssignment{offer("a", 7)})
	if len(ids) != 1 {
		t.Fatalf("offered %v; want the one available", ids)
	}

	// The broker answers with the same grant twice. The production merge
	// is what spends the pairing -- the previous form of this test
	// deleted from the map itself and asserted the deletion, which is
	// Go's delete builtin under test, not this package: the merge could
	// stop spending pairings and every test here stayed green while one
	// CI job was admitted twice.
	var out Message
	mergeAcquired(&out, []int64{7, 7}, byID)
	if len(out.Acquired) != 1 {
		t.Fatalf("acquired %d workloads from a duplicated grant; want 1 -- the job would be admitted twice", len(out.Acquired))
	}
	if len(out.StrandedGrants) != 1 || out.StrandedGrants[0] != 7 {
		t.Errorf("stranded = %v; want the duplicate named, which is what makes it visible", out.StrandedGrants)
	}
}

// TestSessionConflictIsRecognisedByStatusAndByProse. The broker's refusal
// to open a second session is what makes a restart after a crash wait
// instead of failing, and the whole path hangs on recognising it. The
// status is the library's own rendering; the sentence is GitHub's, and
// resting on prose alone would put crash recovery at the mercy of a
// reworded server message. Either must be enough on its own.
func TestSessionConflictIsRecognisedByStatusAndByProse(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"status alone": {
			err:  errors.New(`request POST https://x/sessions failed(status="409 Conflict", activity_id="a")`),
			want: true,
		},
		"prose alone": {
			err:  errors.New("runner scale set already has an active session"),
			want: true,
		},
		"both": {
			err:  errors.New(`failed(status="409 Conflict"): scale set already has an active session`),
			want: true,
		},
		"another conflict-free failure": {
			err:  errors.New(`request POST https://x/sessions failed(status="500 Internal Server Error")`),
			want: false,
		},
		"a 409 mentioned in passing is still a 409 on this path": {
			err:  errors.New(`failed(status="409 Conflict")`),
			want: true,
		},
		"nil": {err: nil, want: false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := IsSessionConflict(tc.err); got != tc.want {
				t.Errorf("IsSessionConflict(%v) = %v; want %v", tc.err, got, tc.want)
			}
		})
	}
}
