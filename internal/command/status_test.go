package command

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rhobuild/runpool/internal/store"
)

// TestProviderReachSeparatesIdleFromUnreachable pins the distinction the
// report exists to make. An instance holding no leases is either idle or
// reaching nothing, and every other field in the document reads the same
// in both cases.
func TestProviderReachSeparatesIdleFromUnreachable(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		contact store.ProviderContact
		want    []string
		unwant  []string
	}{
		"never served": {
			contact: store.ProviderContact{},
			want:    []string{"no contact recorded"},
			unwant:  []string{"failing"},
		},
		"reaching its provider": {
			contact: store.ProviderContact{LastContact: now.Add(-30 * time.Second)},
			want:    []string{"last reached", "30s ago"},
			unwant:  []string{"failing"},
		},
		"failing in the same instant it last reached": {
			contact: store.ProviderContact{
				LastContact: now.Add(-time.Second),
				LastError:   "broker unreachable",
				LastErrorAt: now.Add(-time.Second),
			},
			want: []string{"failing since", "broker unreachable"},
		},
		"failing after having reached it": {
			contact: store.ProviderContact{
				LastContact: now.Add(-2 * time.Hour),
				LastError:   "401 Unauthorized",
				LastErrorAt: now.Add(-1 * time.Minute),
			},
			want: []string{"failing since", "1m0s ago", "last reached 2h0m0s ago", "401 Unauthorized"},
		},
		"failing without ever having reached it": {
			contact: store.ProviderContact{
				LastError:   "no such host",
				LastErrorAt: now.Add(-5 * time.Second),
			},
			want: []string{"failing since", "never reached", "no such host"},
		},
		"recovered since the last failure": {
			contact: store.ProviderContact{
				LastContact: now.Add(-10 * time.Second),
				LastError:   "connection refused",
				LastErrorAt: now.Add(-1 * time.Hour),
			},
			want:   []string{"last reached", "10s ago"},
			unwant: []string{"failing", "connection refused"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := providerReach(tc.contact, now)
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("%q does not contain %q", got, want)
				}
			}
			for _, unwant := range tc.unwant {
				if strings.Contains(got, unwant) {
					t.Errorf("%q contains %q and should not", got, unwant)
				}
			}
		})
	}
}

// TestNoStateAnswersInTheFormAsked: the first scripted call after an
// install meets a state directory the controller has never written.
// Answering --json with a line of prose and exit 0 is a parse failure
// that looks like success, which is the worst shape a first contact can
// take.
func TestNoStateAnswersInTheFormAsked(t *testing.T) {
	var out bytes.Buffer
	if err := reportNoState(IO{Out: &out}, true); err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("--json produced %q, which is not a document: %v", out.String(), err)
	}
	if doc["served"] != false {
		t.Errorf("document = %v; want it to say this instance has not served", doc)
	}
	if doc["api_version"] != statusAPIVersion {
		t.Errorf("document = %v; want the same version the served document carries", doc)
	}

	out.Reset()
	if err := reportNoState(IO{Out: &out}, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "has not run yet") {
		t.Errorf("human report = %q", out.String())
	}
	if strings.HasPrefix(strings.TrimSpace(out.String()), "{") {
		t.Errorf("human report = %q; want prose, not a document", out.String())
	}
}
