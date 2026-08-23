package capsule

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/rhobuild/runpool/internal/assignment"
	"github.com/rhobuild/runpool/internal/platform/docker"
)

// recorder is the intent saga, remembering what it was told. Every
// object is planned before it exists and confirmed after, so a crash
// anywhere leaves an intent whose name either finds the object or
// proves its absence.
type recorder struct {
	planned   []string
	creating  int
	confirmed map[assignment.ResourceIntentID]string
}

func (r *recorder) Plan(kind, role, name string) (assignment.ResourceIntentID, error) {
	r.planned = append(r.planned, kind+"/"+role+"/"+name)
	return assignment.ResourceIntentID(len(r.planned)), nil
}

func (r *recorder) Creating(assignment.ResourceIntentID) error { r.creating++; return nil }

func (r *recorder) Confirm(id assignment.ResourceIntentID, objectID string) error {
	if r.confirmed == nil {
		r.confirmed = map[assignment.ResourceIntentID]string{}
	}
	r.confirmed[id] = objectID
	return nil
}

// TestOnlyATakenNameIsResolved: a create that failed for any other
// reason says nothing about whether an object exists.
//
// Looking one up anyway had two costs. The failure it reported was the
// lookup's, under the name of the create — so a daemon that had gone
// away was reported as "create network …: <an inspect error>". And an
// object adopted after an unknown failure is adopted on a guess, which
// is the one thing this saga exists not to do: nothing is lost by
// refusing, because the intent is already durable and in its creating
// state, and recovery resolves it by name.
func TestOnlyATakenNameIsResolved(t *testing.T) {
	taken := fmt.Errorf("create: %w", docker.ErrAlreadyExists)
	gone := errors.New("Cannot connect to the Docker daemon")

	for name, testCase := range map[string]struct {
		createErr error
		existing  string
		resolved  bool
		want      string
	}{
		"a taken name is resolved and adopted": {
			createErr: taken, existing: "object-1", resolved: true,
		},
		"a taken name with nothing behind it fails": {
			createErr: taken, resolved: true, want: "already exists",
		},
		"an unreachable daemon is not resolved": {
			createErr: gone, resolved: false, want: "Cannot connect",
		},
	} {
		t.Run(name, func(t *testing.T) {
			rec := &recorder{}
			asked := false
			id, err := (&Launcher{}).create(t.Context(), rec, "network", "capsule-net", "runpool-net-1",
				func() (string, error) { return "", testCase.createErr },
				func() (string, error) { asked = true; return testCase.existing, nil })

			if asked != testCase.resolved {
				t.Errorf("the name was looked up = %v; want %v", asked, testCase.resolved)
			}
			if testCase.want == "" {
				if err != nil {
					t.Fatalf("adopting the existing object failed: %v", err)
				}
				if id != testCase.existing {
					t.Errorf("adopted %q; the object that exists is %q", id, testCase.existing)
				}
				if rec.confirmed[1] != testCase.existing {
					t.Error("the intent was not confirmed with the object that was adopted")
				}
				return
			}
			if err == nil {
				t.Fatalf("a create that failed returned %q", id)
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Errorf("failed with %q; the reason has to carry %q", err, testCase.want)
			}
		})
	}
}

// TestTheIntentIsDurableBeforeTheObjectExists: planned, then creating,
// then the effect. A crash between any two of those leaves an intent
// whose deterministic name either finds the object or proves it absent,
// which is what makes a leaked object impossible rather than unlikely.
func TestTheIntentIsDurableBeforeTheObjectExists(t *testing.T) {
	rec := &recorder{}
	created := false
	if _, err := (&Launcher{}).create(t.Context(), rec, "volume", "dind-data", "runpool-dind-1",
		func() (string, error) {
			if len(rec.planned) != 1 || rec.creating != 1 {
				t.Error("the object was created before its intent was durable")
			}
			created = true
			return "volume-1", nil
		},
		func() (string, error) { t.Error("a successful create was looked up"); return "", nil },
	); err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("nothing was created")
	}
	if rec.confirmed[1] != "volume-1" {
		t.Error("the intent was not confirmed with what was created")
	}
}
