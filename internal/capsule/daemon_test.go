package capsule

import (
	"context"

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
}

func (f *fakeDaemon) ContainerStatus(_ context.Context, id string) (engine.ContainerState, error) {
	return f.status(id)
}

func (f *fakeDaemon) Exec(_ context.Context, id string, cmd []string) (int, string, error) {
	return f.exec(id, cmd)
}
