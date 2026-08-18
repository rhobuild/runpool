// Package protocol is the capsule control surface's shared vocabulary:
// the values the supervisor inside a capsule and the launcher outside it
// must agree on. Neither side can import the other — the supervisor is
// the capsule image's PID 1 and the launcher drives it over exec — so
// before this package each value was declared twice with a comment
// naming its counterpart, and a script compared the declarations in CI.
// A leaf package both sides import makes the drift unrepresentable
// instead of detectable: there is one declaration, so there is nothing
// to compare.
//
// Only the values live here. The files they travel through, the verbs
// spoken over exec and the machinery around them belong to their sides;
// this package must stay dependency-free so the supervisor's static
// binary carries nothing it does not run.
package protocol

// Version is the control protocol this build speaks. The supervisor
// writes it to the control directory at boot, before it is asked
// anything, and the launcher reads it before handing the capsule
// anything: a capsule that declares a version this build does not speak
// is refused, not retried.
const Version = "1"

// AbortedExitCode is the status the supervisor exits with when it stops
// before handing the job to the runner: the job was never handed over
// and must be retried.
//
// It is a distinct exit code rather than a state-file value because the
// state file dies with the container: by the time a controller inspects
// an aborted capsule, the daemon reports `exited` and nothing inside can
// be read. Without it, every abort read as an ordinary exit — and an
// attempt that never ran was settled as complete and never requeued.
const AbortedExitCode = 79
