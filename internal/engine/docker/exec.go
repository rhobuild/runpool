package docker

import (
	"bytes"
	"context"
	"io"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"
)

// closeOnDone closes a hijacked connection when ctx ends, and returns
// the function that stops watching.
//
// The client's context governs the dial and the protocol upgrade and
// stops there: once the connection is hijacked nothing consults ctx
// again, so a read blocks for as long as the container takes to answer,
// and the deferred close cannot help — it is deferred inside the call
// that is blocked. An exec into a wedged container is otherwise
// unbounded, and one of those is held across the sandbox's refresh
// lock, which every launch waits on with a mutex that takes no context.
func closeOnDone(ctx context.Context, attach client.ExecAttachResult) func() {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			attach.Close()
		case <-done:
		}
	}()
	return func() { close(done) }
}

// demux drains the multiplexed stream, reporting the context's error
// when the context is what ended the read. Without that the caller sees
// "use of closed network connection" and cannot tell a timeout it set
// from a daemon that broke.
func demux(ctx context.Context, attach client.ExecAttachResult, buf *bytes.Buffer) error {
	if _, err := stdcopy.StdCopy(buf, buf, attach.Reader); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return err
	}
	return nil
}

// Exec runs a command inside a running container and returns its exit
// code and combined output. The capsule uses it to read the dind socket
// gid and to probe daemon readiness.
func (c *Client) Exec(ctx context.Context, containerID string, cmd []string) (int, string, error) {
	return c.exec(ctx, containerID, cmd, nil)
}

// ExecWithInput runs a command inside a running container feeding input
// on its stdin, and returns its exit code and combined output. It is
// the credential channel: stdin is the one path into a container that
// Docker persists nowhere — not in the container config, not in an
// image layer, not in the log driver.
func (c *Client) ExecWithInput(ctx context.Context, containerID string, cmd []string, input []byte) (int, string, error) {
	return c.exec(ctx, containerID, cmd, bytes.NewReader(input))
}

// exec owns the common Docker exec lifecycle. A nil input leaves stdin
// detached; a non-nil reader is copied completely and then half-closed so the
// process observes EOF before its output is drained.
func (c *Client) exec(ctx context.Context, containerID string, cmd []string, input io.Reader) (int, string, error) {
	created, err := c.cli.ExecCreate(ctx, containerID, client.ExecCreateOptions{
		Cmd:          cmd,
		AttachStdin:  input != nil,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return -1, "", err
	}
	attach, err := c.cli.ExecAttach(ctx, created.ID, client.ExecAttachOptions{})
	if err != nil {
		return -1, "", err
	}
	defer attach.Close()
	// Deferred after the close so it unwinds first: stop watching, then
	// close.
	defer closeOnDone(ctx, attach)()

	if input != nil {
		if _, err := io.Copy(attach.Conn, input); err != nil {
			return -1, "", err
		}
		if err := attach.CloseWrite(); err != nil {
			return -1, "", err
		}
	}

	var buf bytes.Buffer
	if err := demux(ctx, attach, &buf); err != nil {
		return -1, buf.String(), err
	}
	inspect, err := c.cli.ExecInspect(ctx, created.ID, client.ExecInspectOptions{})
	if err != nil {
		return -1, buf.String(), err
	}
	return inspect.ExitCode, buf.String(), nil
}
