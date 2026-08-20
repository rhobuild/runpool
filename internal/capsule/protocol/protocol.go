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
// Only the values live here, and the one predicate that decides what a
// value means. The files they travel through, the verbs spoken over exec
// and the machinery around them belong to their sides; this package
// depends on nothing outside the standard library so the supervisor's
// static binary carries nothing it does not run.
package protocol

import "strings"

// Version is the control protocol this build speaks. The supervisor
// writes it to the control directory at boot, before it is asked
// anything, and the launcher reads it before handing the capsule
// anything: a capsule that declares a version this build does not speak
// is refused, not retried.
//
// Version 2 moved when StateWaiting is written. A version 1 supervisor
// wrote it at boot, ahead of its daemon, so a launcher that treats the
// state as readiness would deliver a credential and authorize a start
// against a capsule whose daemon may never come up. The two builds are
// not interchangeable in either direction, which is what the equality
// check enforces.
const Version = "2"

// The states a supervisor-family container writes to its control
// directory. The capsule's supervisor and the gateway are the same
// binary in two containers and share this vocabulary; the launcher waits
// on these names from the outside, so both sides read one declaration.
const (
	// StateBooting is the control surface answering before anything it
	// supervises is proven. Nothing may be handed to a capsule here.
	StateBooting = "booting"
	// StateWaiting is the daemon proven ready and the runner deliberately
	// not started: the state in which a credential is delivered and a
	// start is authorized.
	StateWaiting = "waiting"
	// StateRunning is written once fork/exec has returned, so its
	// presence is the proof that the job was handed over — not that the
	// supervisor was about to try. Writing it before the exec would make
	// a fork that failed indistinguishable from a runner that took the
	// job and died, and the two settle an attempt differently.
	StateRunning = "running"
	// StateReady is the gateway's: ruleset installed and relay listening.
	StateReady = "ready"
)

// The prefixes a terminal state carries, each with its reason appended.
// Which one it is decides whether the job may run again, so they are
// declared once rather than spelled at each side.
const (
	// ExitedPrefix carries the runner's own status.
	ExitedPrefix = "exited:"
	// FailedPrefix is a failure after the runner started: an execution
	// outcome.
	FailedPrefix = "failed:"
	// AbortedPrefix is a failure before it started: the job was never
	// handed over and must be retried.
	AbortedPrefix = "aborted:"
)

// Terminal reports whether a state says the supervisor will never reach
// another one. A caller waiting for a state it can no longer reach must
// give up with the reason rather than poll out its deadline: the reason
// is written here and lost when the container goes.
func Terminal(state string) bool {
	return strings.HasPrefix(state, ExitedPrefix) ||
		strings.HasPrefix(state, FailedPrefix) ||
		strings.HasPrefix(state, AbortedPrefix)
}

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

// GatewayDenyAllCommand is the supervisor subcommand that revokes a
// gateway's own egress policy from inside it.
//
// It lives here for the reason the rest of this package does: the
// controller spells it to ask and the supervisor spells it to answer,
// and they are two binaries. A typo on either side is a gateway that
// keeps relaying while the exec reports a failure the caller logs and
// moves past — which is the shape of every other value this package
// exists to keep from being written twice.
const GatewayDenyAllCommand = "gateway-deny-all"
