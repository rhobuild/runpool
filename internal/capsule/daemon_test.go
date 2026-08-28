package capsule

import (
	"context"
	"errors"

	"github.com/rhobuild/runpool/internal/engine"
)

// fakeDaemon answers the one or two calls a test cares about. The
// embedded interface is nil on purpose: a method this fake was not given
// panics rather than returning a zero value, so a test that reaches
// further than it declared says so instead of passing on a lie.
type fakeDaemon struct {
	capsuleDaemon
	status func(id string) (engine.ContainerState, error)
	exec   func(id string, cmd []string) (int, string, error)
	// logs is what the container said. Unset is a daemon whose logs
	// could not be read, which is the case every error message here was
	// written against: the tail is an addition to a diagnosis, never the
	// diagnosis, so a test that does not set it still asserts the whole
	// message an operator would see without one.
	logs func(id string, lines int) (string, error)
}

func (f *fakeDaemon) ContainerStatus(_ context.Context, id string) (engine.ContainerState, error) {
	return f.status(id)
}

func (f *fakeDaemon) Exec(_ context.Context, id string, cmd []string) (int, string, error) {
	return f.exec(id, cmd)
}

func (f *fakeDaemon) TailLogs(_ context.Context, id string, lines int) (string, error) {
	if f.logs == nil {
		return "", errors.New("no logs")
	}
	return f.logs(id, lines)
}
