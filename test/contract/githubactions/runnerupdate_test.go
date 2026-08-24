package githubcontract

import (
	"context"
	"testing"
	"time"

	"github.com/actions/scaleset"
)

// TestTheScaleSetForbidsRunnerSelfUpdate.
//
// A scale set that permits self-update tells its runners to upgrade
// before taking work. The capsule image pins the runner by digest and the
// image lock records that digest as the reviewed input, so a runner that
// replaces its own binary makes both false. Under the restricted network
// profile it does not even get that far: the egress policy refuses the
// download and the runner fails before the job begins.
//
// The setting is only observable from the created scale set, so this
// creates one through the production adapter and reads it back.
func TestTheScaleSetForbidsRunnerSelfUpdate(t *testing.T) {
	url, token := target(t, envRepoURL, envRepoToken)
	gh := newWrapper(t, url, token)
	raw := newClient(t, url, token)

	set := ensureSet(t, gh, uniqueName(t))

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	created, err := raw.GetRunnerScaleSetByID(ctx, set.ID)
	if err != nil {
		t.Fatalf("read back scale set %d: %v", set.ID, err)
	}
	if created == nil {
		t.Fatalf("scale set %d was created and cannot be read back", set.ID)
	}

	if !created.RunnerSetting.DisableUpdate {
		t.Errorf("scale set %q permits runner self-update; a runner that replaces its "+
			"own binary is not the digest the capsule image pins, and under the "+
			"restricted profile the attempt fails the runner before its job starts",
			created.Name)
	}
}

// TestAnAdoptedScaleSetForbidsRunnerSelfUpdate is the same guarantee on
// the path every start after the first takes.
//
// The setting belongs to the scale set, not to the act of creating one,
// so a set this instance created under a build that did not ask for it
// keeps permitting self-update for as long as it is adopted. This creates
// a set with the setting off, adopts it by its recorded id the way a
// restart does, and reads back what the provider now holds.
func TestAnAdoptedScaleSetForbidsRunnerSelfUpdate(t *testing.T) {
	url, token := target(t, envRepoURL, envRepoToken)
	gh := newWrapper(t, url, token)
	raw := newClient(t, url, token)

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	group, err := raw.GetRunnerGroupByName(ctx, scaleset.DefaultRunnerGroup)
	if err != nil {
		t.Fatalf("resolve the default runner group: %v", err)
	}
	// Created directly, so it starts in the state an older build left
	// behind: self-update permitted.
	permissive := createScaleSet(t, raw, group, uniqueName(t))
	if permissive.RunnerSetting.DisableUpdate {
		t.Fatal("the fixture set was created with self-update already forbidden; " +
			"this test cannot tell adoption from creation")
	}

	adopted, err := gh.EnsureScaleSet(ctx, "", permissive.Name, permissive.ID, false, adoption(t).record)
	if err != nil {
		t.Fatalf("adopt scale set %q: %v", permissive.Name, err)
	}
	if !adopted.Adopted {
		t.Fatalf("scale set %q was created rather than adopted; the id was recorded",
			permissive.Name)
	}

	current, err := raw.GetRunnerScaleSetByID(ctx, adopted.ID)
	if err != nil {
		t.Fatalf("read back scale set %d: %v", adopted.ID, err)
	}
	if current == nil {
		t.Fatalf("scale set %d was adopted and cannot be read back", adopted.ID)
	}
	if !current.RunnerSetting.DisableUpdate {
		t.Errorf("adopted scale set %q still permits runner self-update; every start "+
			"after the first takes this path, so the guarantee would hold only on "+
			"a set created by this build and never afterwards", current.Name)
	}
}

// TestAdoptionLeavesACorrectScaleSetAlone is the other half of the
// adoption guarantee: the setting is asserted, not re-written. A set that
// already forbids self-update is adopted with no PATCH - the read that
// adoption already makes carries the answer - so a restart's startup does
// not depend on a write call the healthy path never needed.
func TestAdoptionLeavesACorrectScaleSetAlone(t *testing.T) {
	url, token := target(t, envRepoURL, envRepoToken)
	gh := newWrapper(t, url, token)
	raw := newClient(t, url, token)

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	group, err := raw.GetRunnerGroupByName(ctx, scaleset.DefaultRunnerGroup)
	if err != nil {
		t.Fatalf("resolve the default runner group: %v", err)
	}
	name := uniqueName(t)
	created, err := raw.CreateRunnerScaleSet(ctx, &scaleset.RunnerScaleSet{
		Name:          name,
		RunnerGroupID: group.ID,
		RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true},
	})
	if err != nil {
		t.Fatalf("create scale set %q: %v", name, err)
	}
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()
		if err := raw.DeleteRunnerScaleSet(cctx, created.ID); err != nil {
			t.Errorf("cleanup: delete scale set %d: %v", created.ID, err)
		}
	})

	adopted, err := gh.EnsureScaleSet(ctx, "", name, created.ID, false, adoption(t).record)
	if err != nil {
		t.Fatalf("adopt scale set %q: %v", name, err)
	}
	if !adopted.Adopted {
		t.Fatalf("scale set %q was created rather than adopted", name)
	}
	if !adopted.DisableUpdate {
		t.Error("the adopted set reports self-update permitted; the setting the read " +
			"carried was dropped on the way out, so adoption cannot tell a correct " +
			"set from one it must fix")
	}
}
