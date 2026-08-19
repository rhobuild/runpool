package capsule

import (
	"strconv"
	"strings"
	"testing"
)

// TestProtocolVerdict: the capsule states the protocol it speaks, and a
// capsule that states something else is refused before it is handed a
// credential. Without this the mismatch arrives as a job that failed
// instead of an image that cannot be used.
func TestProtocolVerdict(t *testing.T) {
	// The neighbouring versions are derived from the one this build
	// speaks, never written down: a table naming them by hand starts
	// passing for the wrong reason on the day one of them ships, which
	// is the same day the refusal matters most.
	n, err := strconv.Atoi(ProtocolVersion)
	if err != nil {
		t.Fatalf("control protocol version %q is not a number", ProtocolVersion)
	}
	older, newer := strconv.Itoa(n-1), strconv.Itoa(n+1)

	for name, tc := range map[string]struct {
		code int
		out  string
		want string // empty means the capsule is accepted
	}{
		"the version this build speaks":   {0, ProtocolVersion, ""},
		"trailing newline from the file":  {0, ProtocolVersion + "\n", ""},
		"surrounding whitespace":          {0, "  " + ProtocolVersion + "  \n", ""},
		"an older supervisor":             {0, older + "\n", "not a pair"},
		"a newer supervisor":              {0, newer + "\n", "not a pair"},
		"no protocol file at all":         {1, "cat: /run/runpool/protocol: No such file or directory", "declares no control protocol"},
		"an empty file":                   {0, "\n", "not a pair"},
		"a file the read could not parse": {0, "one", "not a pair"},
	} {
		t.Run(name, func(t *testing.T) {
			err := protocolVerdict(tc.code, tc.out)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("accepted capsule refused: %v", err)
			case tc.want == "":
				return
			case err == nil:
				t.Fatalf("capsule accepted; want a refusal containing %q", tc.want)
			case !strings.Contains(err.Error(), tc.want):
				t.Errorf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

// TestTheLauncherOwnsTheGatewayImage: the container that applies a job's
// egress policy runs what the launcher was built with, and a Spec cannot
// name it. That is the point — the field it used to share with the
// capsule made "extend the image my jobs run in" mean "replace what
// confines them", and a required field a caller can leave empty is a
// caller who eventually does. The live contract suite exercises the
// container this decides.
func TestTheLauncherOwnsTheGatewayImage(t *testing.T) {
	const shipped = "ghcr.io/rhobuild/runpool/capsule@sha256:" +
		"1111111111111111111111111111111111111111111111111111111111111111"
	if got := NewLauncher(nil, shipped).gatewayImage; got != shipped {
		t.Errorf("gateway image = %q, want the one the launcher was built with", got)
	}
}
