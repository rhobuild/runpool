package githubcontract

import (
	"context"
	"testing"
	"time"
)

// TestOrganizationDefaultGroupScaleSet verifies creation and adoption in an
// organization's Default runner group, plus the message session's initial
// statistics snapshot.
func TestOrganizationDefaultGroupScaleSet(t *testing.T) {
	url, token := target(t, envOrgURL, envOrgToken)
	gh := newWrapper(t, url, token)

	name := uniqueName(t)
	created := ensureSet(t, gh, name)
	if created.Adopted {
		t.Fatalf("fresh scale set reported as adopted: %+v", created)
	}
	t.Logf("created scale set: id=%d group=%d (%s)", created.ID, created.GroupID, created.GroupName)

	// A second ensure adopts only when the caller proves ownership with
	// the id it recorded; the restart path.
	again, err := gh.EnsureScaleSet(testCtx(t), "", name, created.ID, false, adoption(t).record)
	if err != nil {
		t.Fatalf("re-ensure: %v", err)
	}
	if !again.Adopted || again.ID != created.ID {
		t.Fatalf("re-ensure = %+v; want adoption of id %d", again, created.ID)
	}

	session, err := gh.OpenSession(testCtx(t), created.ID, "runpool-contract")
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := session.Close(ctx); err != nil {
			t.Errorf("close message session for scale set %d: %v", created.ID, err)
		}
	})

	initial := session.Initial()
	if initial == nil {
		t.Fatal("session carries no initial statistics snapshot")
	}
	if initial.Assigned != 0 || initial.RegisteredRunners != 0 {
		t.Errorf("fresh scale set statistics not zero: %+v", initial)
	}
	t.Logf("session open; initial statistics %+v", initial)
}
