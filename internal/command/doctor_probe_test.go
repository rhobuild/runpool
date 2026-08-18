package command

import (
	"context"
	"testing"

	"github.com/rhobuild/runpool/internal/doctor"
)

// The probe the doctor receives must carry exactly the two reads the
// doctor declares and nothing of the adapter's mutating surface. The
// doctor promises it never mutates durable product state; an embedded
// client would promote DeleteScaleSet and RemoveRunner onto the value
// that crosses that boundary, and the promotion is invisible at the
// call site — the interface still satisfies, the extra methods ride
// along. Asserting the absence here is what keeps the narrowing real.
func TestTheDoctorProbeCannotMutate(t *testing.T) {
	var probe any = githubProbe{}

	if _, ok := probe.(doctor.CredentialProbe); !ok {
		t.Fatal("githubProbe no longer satisfies the doctor's CredentialProbe")
	}
	if _, ok := probe.(interface {
		DeleteScaleSet(ctx context.Context, id int) error
	}); ok {
		t.Fatal("the doctor's probe exposes DeleteScaleSet; the client must be a named field, not embedded")
	}
	if _, ok := probe.(interface {
		RemoveRunner(ctx context.Context, id int) error
	}); ok {
		t.Fatal("the doctor's probe exposes RemoveRunner; the client must be a named field, not embedded")
	}
}
