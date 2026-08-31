package assignment

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"
)

func fingerprintFor(t *testing.T, payload []WorkloadAssignment,
	format DeliveryFingerprintFormat) PayloadFingerprint {
	t.Helper()
	digest, ok := DeliveryFingerprintForFormat(payload, format)
	if !ok {
		t.Fatalf("fingerprint format %q is unavailable", format)
	}
	return digest
}

func TestFingerprintEncodingsAreNamedContracts(t *testing.T) {
	payload := []WorkloadAssignment{{
		SourceWorkloadKey: "job-old", TenantKey: "acme", ProjectKey: "app",
		SourceRequestID: 7, SourceRunID: 41, Labels: []string{"self-hosted", "linux"},
	}}
	for format, want := range map[DeliveryFingerprintFormat]string{
		FingerprintFormatDelimiterSeparatedSHA256: "afa259201b084fbd9ae269769c205995d2a480c0616aa17a1aceb514dec8211e",
		FingerprintFormatLengthPrefixedSHA256:     "122fb51719df68053f6e058ead239c213b790d5699fdbca288a9008d7d40d55f",
	} {
		digest := fingerprintFor(t, payload, format)
		if got := hex.EncodeToString(digest[:]); got != want {
			t.Errorf("fingerprint %q = %s; want immutable encoding %s", format, got, want)
		}
	}
}

// The fingerprint must be insensitive to arrival order — the provider
// owes no ordering guarantee — and sensitive to every field the domain
// processes, because drift in an uncovered field would slip past the
// fail-closed comparison as if the redelivery were exact.
func TestFingerprintIsCanonical(t *testing.T) {
	a := WorkloadAssignment{SourceWorkloadKey: "job-a", TenantKey: "acme",
		ProjectKey: "app", SourceRequestID: 1, SourceRunID: 10, Labels: []string{"x", "y"}}
	b := WorkloadAssignment{SourceWorkloadKey: "job-b", TenantKey: "acme",
		ProjectKey: "app", SourceRequestID: 2, SourceRunID: 10, Labels: []string{"y", "x"}}

	for format := range deliveryFingerprintEncoders {
		forwardDigest := fingerprintFor(t, []WorkloadAssignment{a, b}, format)
		swappedDigest := fingerprintFor(t, []WorkloadAssignment{b, a}, format)
		if forwardDigest != swappedDigest {
			t.Errorf("fingerprint %q depends on assignment order", format)
		}
	}

	bLabels := b
	bLabels.Labels = []string{"x", "y"}
	for format := range deliveryFingerprintEncoders {
		forwardDigest := fingerprintFor(t, []WorkloadAssignment{a, b}, format)
		swappedDigest := fingerprintFor(t, []WorkloadAssignment{a, bLabels}, format)
		if forwardDigest != swappedDigest {
			t.Errorf("fingerprint %q depends on label order", format)
		}
	}

	mutations := []func(*WorkloadAssignment){
		func(w *WorkloadAssignment) { w.SourceWorkloadKey = "job-mut" },
		func(w *WorkloadAssignment) { w.TenantKey = "other" },
		func(w *WorkloadAssignment) { w.ProjectKey = "other" },
		func(w *WorkloadAssignment) { w.SourceRequestID = 99 },
		func(w *WorkloadAssignment) { w.SourceRunID = 99 },
		func(w *WorkloadAssignment) { w.Labels = []string{"z"} },
	}
	for i, mutate := range mutations {
		mutated := a
		mutate(&mutated)
		for format := range deliveryFingerprintEncoders {
			originalDigest := fingerprintFor(t, []WorkloadAssignment{a, b}, format)
			changedDigest := fingerprintFor(t, []WorkloadAssignment{mutated, b}, format)
			if originalDigest == changedDigest {
				t.Errorf("mutation %d did not change fingerprint %q; that field is uncovered", i, format)
			}
		}
	}
}

// TestCanonicalFingerprintSeparatesDelimitedAmbiguities pins the reason for
// the new encoding. These payloads are structurally distinct but produced the
// same delimiter-based preimage.
func TestCanonicalFingerprintSeparatesDelimitedAmbiguities(t *testing.T) {
	cases := []struct {
		name        string
		left, right []WorkloadAssignment
	}{
		{
			name:  "label boundary",
			left:  []WorkloadAssignment{{SourceWorkloadKey: "job", Labels: []string{"a,b"}}},
			right: []WorkloadAssignment{{SourceWorkloadKey: "job", Labels: []string{"a", "b"}}},
		},
		{
			name:  "field boundary",
			left:  []WorkloadAssignment{{SourceWorkloadKey: "job|tenant", TenantKey: "project"}},
			right: []WorkloadAssignment{{SourceWorkloadKey: "job", TenantKey: "tenant|project"}},
		},
		{
			name:  "assignment boundary",
			left:  []WorkloadAssignment{{SourceWorkloadKey: "a|||0|0|\nb"}},
			right: []WorkloadAssignment{{SourceWorkloadKey: "a"}, {SourceWorkloadKey: "b"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			leftDelimited := fingerprintDelimited(tc.left)
			rightDelimited := fingerprintDelimited(tc.right)
			if leftDelimited != rightDelimited {
				t.Fatal("fixture no longer demonstrates a delimiter ambiguity")
			}
			if bytes.Equal(canonicalFingerprintPreimage(tc.left), canonicalFingerprintPreimage(tc.right)) {
				t.Fatal("distinct payloads have the same canonical encoding")
			}
		})
	}
}

func TestFingerprintFormatSelectionIsTotal(t *testing.T) {
	payload := []WorkloadAssignment{{SourceWorkloadKey: "job"}}
	for format := range deliveryFingerprintEncoders {
		if _, ok := DeliveryFingerprintForFormat(payload, format); !ok {
			t.Errorf("supported fingerprint format %q is unavailable", format)
		}
	}
	if _, ok := DeliveryFingerprintForFormat(payload, ""); ok {
		t.Error("an empty format was accepted")
	}
	if _, ok := DeliveryFingerprintForFormat(payload, "future-encoding-sha256"); ok {
		t.Error("an unknown format was accepted")
	}
	if _, ok := deliveryFingerprintEncoders[currentDeliveryFingerprintFormat]; !ok {
		t.Error("the current fingerprint format has no encoder")
	}
	for _, format := range DeliveryFingerprintFormats() {
		if format == "" {
			t.Error("the fingerprint registry contains an unnamed format")
		}
	}
}

func FuzzCanonicalFingerprintFieldBoundaries(f *testing.F) {
	f.Add("job", "tenant", "project", "label")
	f.Add("a|b", "x\ny", "", "α,β")
	f.Fuzz(func(t *testing.T, workload, tenant, project, label string) {
		left := []WorkloadAssignment{{
			SourceWorkloadKey: SourceWorkloadKey(workload), TenantKey: TenantKey(tenant),
			ProjectKey: ProjectKey(project),
			Labels:     []string{label},
		}}
		fields := []string{workload, tenant, project, label}
		for i := range fields {
			rightFields := append([]string(nil), fields...)
			rightFields[i] += "\x00"
			right := []WorkloadAssignment{{
				SourceWorkloadKey: SourceWorkloadKey(rightFields[0]), TenantKey: TenantKey(rightFields[1]),
				ProjectKey: ProjectKey(rightFields[2]), Labels: []string{rightFields[3]},
			}}
			if bytes.Equal(canonicalFingerprintPreimage(left), canonicalFingerprintPreimage(right)) {
				t.Fatalf("changing field %d did not change the canonical encoding", i)
			}
		}
	})
}

func TestDeliveryKeyNamesItsFormat(t *testing.T) {
	if got := NewDeliveryKey(7, 41); got != "queue-delivery|7|41" {
		t.Errorf("NewDeliveryKey(7, 41) = %q; the named encoding is the stored identity", got)
	}
}

// A delivery id is unique within the queue that issued it, and a binding
// outlives its queue: a replacement scale set numbers from the start
// again. Keyed on the id alone, message 41 of the new queue would be the
// delivery the binding already recorded and acknowledged from the old
// one, and the store would deduplicate away work it has never seen.
func TestDeliveryKeySeparatesQueues(t *testing.T) {
	if NewDeliveryKey(7, 41) == NewDeliveryKey(8, 41) {
		t.Error("two queues issuing the same delivery id produced one key; the second delivery would be deduplicated away")
	}
}

func TestVocabularySnapshotsCannotMutateTheDomain(t *testing.T) {
	resolutions := Resolutions()
	wantResolution := resolutions[0]
	resolutions[0] = "caller_mutation"
	if got := Resolutions()[0]; got != wantResolution {
		t.Errorf("Resolutions()[0] = %q after caller mutation; want %q", got, wantResolution)
	}

	observations := ExecutionObservations()
	wantObservation := observations[0]
	observations[0] = "caller_mutation"
	if got := ExecutionObservations()[0]; got != wantObservation {
		t.Errorf("ExecutionObservations()[0] = %q after caller mutation; want %q", got, wantObservation)
	}
}

// TestAnAssignmentWithoutAWorkloadKeyIsRefused: the key is what
// deduplication and recovery recognise an assignment by, so recording
// one without it plants a row nothing can recognise again. The guard is
// the first thing persistDelivery runs; without it the schema's own
// constraint still refuses the row, but as an opaque constraint error on
// a message at the head of an ordered queue — which stays unacknowledged
// and wedges the binding.
func TestAnAssignmentWithoutAWorkloadKeyIsRefused(t *testing.T) {
	bad := WorkloadAssignment{TenantKey: "acme", ProjectKey: "app", SourceRunID: 7}
	err := bad.Validate()
	if err == nil {
		t.Fatal("an assignment with no workload key validated")
	}
	for _, names := range []string{"acme", "app", "7"} {
		if !strings.Contains(err.Error(), names) {
			t.Errorf("error = %q; it has to name %q, which is all an operator has to find the run", err, names)
		}
	}
	good := bad
	good.SourceWorkloadKey = "job-1"
	if err := good.Validate(); err != nil {
		t.Errorf("a complete assignment was refused: %v", err)
	}
}
