package command

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rhobuild/runpool/internal/assignment"
	"github.com/rhobuild/runpool/internal/platform/docker"
)

// wedgedDaemon removes what it is asked to remove, except the objects
// named in refuse. The removals are recorded in order, because the order
// is the invariant: a network or a volume a container still holds open
// cannot go until the container has.
type wedgedDaemon struct {
	maintenanceDaemon
	refuse  map[string]error
	removed []string
}

func (d *wedgedDaemon) remove(kind, reference string) error {
	if err, ok := d.refuse[reference]; ok {
		return err
	}
	d.removed = append(d.removed, kind+" "+reference)
	return nil
}

func (d *wedgedDaemon) RemoveOwnedContainer(_ context.Context, reference string,
	_ assignment.InstanceID, _ assignment.LeaseID) error {
	return d.remove("container", reference)
}

func (d *wedgedDaemon) RemoveOwnedNetwork(_ context.Context, reference string,
	_ assignment.InstanceID, _ assignment.LeaseID) error {
	return d.remove("network", reference)
}

func (d *wedgedDaemon) RemoveOwnedVolume(_ context.Context, reference string,
	_ assignment.InstanceID, _ assignment.LeaseID) error {
	return d.remove("volume", reference)
}

func maintenancePlan() ownedPlan {
	return ownedPlan{
		instanceID: "instance-1",
		containers: []docker.OwnedContainer{{ID: "c1", Name: "runpool-capsule-1"}, {ID: "c2", Name: "runpool-gateway-1"}},
		networks:   []docker.OwnedResource{{ID: "n1"}},
		volumes:    []docker.OwnedResource{{ID: "v1"}},
	}
}

// TestRemovalStopsAtWhatWillNotGo: a removal that failed is reported,
// and nothing after it is claimed.
//
// The order is the invariant — containers first, then what they held
// open — and a container that will not die is the case where continuing
// would report a network removed that the daemon still refuses. A live
// daemon cannot be asked to wedge a container on demand, so until this
// path was reachable through a seam nothing exercised it at all.
func TestRemovalStopsAtWhatWillNotGo(t *testing.T) {
	wedged := errors.New("device or resource busy")

	for name, testCase := range map[string]struct {
		refuse      string
		wantRemoved []string
		wantErr     string
	}{
		"everything goes, containers before what they held open": {
			wantRemoved: []string{"container c1", "container c2", "network n1", "volume v1"},
		},
		"a container that will not die stops the sweep": {
			refuse:      "c1",
			wantRemoved: nil,
			wantErr:     "remove container runpool-capsule-1",
		},
		"a network that will not go stops it after the containers": {
			refuse:      "n1",
			wantRemoved: []string{"container c1", "container c2"},
			wantErr:     "remove network n1",
		},
	} {
		t.Run(name, func(t *testing.T) {
			daemon := &wedgedDaemon{}
			if testCase.refuse != "" {
				daemon.refuse = map[string]error{testCase.refuse: wedged}
			}
			err := maintenancePlan().remove(t.Context(), daemon)

			if testCase.wantErr == "" {
				if err != nil {
					t.Fatalf("a plan the daemon accepted failed: %v", err)
				}
			} else {
				if err == nil {
					t.Fatal("a removal the daemon refused was reported as done")
				}
				if !strings.Contains(err.Error(), testCase.wantErr) {
					t.Errorf("failed with %q; it has to name what would not go (%q)", err, testCase.wantErr)
				}
				if !errors.Is(err, wedged) {
					t.Error("the daemon's own reason has to survive: it is what an operator acts on")
				}
			}
			if strings.Join(daemon.removed, ", ") != strings.Join(testCase.wantRemoved, ", ") {
				t.Errorf("removed %v; want %v", daemon.removed, testCase.wantRemoved)
			}
		})
	}
}
