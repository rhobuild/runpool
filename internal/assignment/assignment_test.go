package assignment

import "testing"

// The fingerprint must be insensitive to arrival order — the provider
// owes no ordering guarantee — and sensitive to every field the domain
// processes, because drift in an uncovered field would slip past the
// fail-closed comparison as if the redelivery were exact.
func TestFingerprintIsCanonical(t *testing.T) {
	a := WorkloadAssignment{SourceWorkloadKey: "job-a", TenantKey: "acme",
		ProjectKey: "app", SourceRequestID: 1, SourceRunID: 10, Labels: []string{"x", "y"}}
	b := WorkloadAssignment{SourceWorkloadKey: "job-b", TenantKey: "acme",
		ProjectKey: "app", SourceRequestID: 2, SourceRunID: 10, Labels: []string{"y", "x"}}

	forward := Fingerprint([]WorkloadAssignment{a, b})
	swapped := Fingerprint([]WorkloadAssignment{b, a})
	if forward != swapped {
		t.Error("fingerprint depends on assignment order; a reordered redelivery would read as drift")
	}

	bLabels := b
	bLabels.Labels = []string{"x", "y"}
	if Fingerprint([]WorkloadAssignment{a, b}) != Fingerprint([]WorkloadAssignment{a, bLabels}) {
		t.Error("fingerprint depends on label order; providers owe no ordering guarantee")
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
		if Fingerprint([]WorkloadAssignment{a, b}) == Fingerprint([]WorkloadAssignment{mutated, b}) {
			t.Errorf("mutation %d did not change the fingerprint; that field is uncovered", i)
		}
	}
}

func TestDeliveryKeyIsVersioned(t *testing.T) {
	if got := DeliveryKey(7, 41); got != "v2|7|41" {
		t.Errorf("DeliveryKey(7, 41) = %q; the versioned encoding is the stored identity", got)
	}
}

// A delivery id is unique within the queue that issued it, and a binding
// outlives its queue: a replacement scale set numbers from the start
// again. Keyed on the id alone, message 41 of the new queue would be the
// delivery the binding already recorded and acknowledged from the old
// one, and the store would deduplicate away work it has never seen.
func TestDeliveryKeySeparatesQueues(t *testing.T) {
	if DeliveryKey(7, 41) == DeliveryKey(8, 41) {
		t.Error("two queues issuing the same delivery id produced one key; the second delivery would be deduplicated away")
	}
}
