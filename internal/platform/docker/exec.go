package docker

import (
	"bytes"
	"context"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"
)

// Exec runs a command inside a running container and returns its exit
// code and combined output. The capsule uses it to read the dind socket
// gid and to probe daemon readiness.
func (c *Client) Exec(ctx context.Context, containerID string, cmd []string) (int, string, error) {
	created, err := c.cli.ExecCreate(ctx, containerID, client.ExecCreateOptions{
		Cmd:          cmd,
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

	var buf bytes.Buffer
	if _, err := stdcopy.StdCopy(&buf, &buf, attach.Reader); err != nil {
		return -1, buf.String(), err
	}
	inspect, err := c.cli.ExecInspect(ctx, created.ID, client.ExecInspectOptions{})
	if err != nil {
		return -1, buf.String(), err
	}
	return inspect.ExitCode, buf.String(), nil
}

// ExecOK reports whether a command inside the container exits zero — the
// readiness-probe shape.
func (c *Client) ExecOK(ctx context.Context, containerID string, cmd []string) bool {
	code, _, err := c.Exec(ctx, containerID, cmd)
	return err == nil && code == 0
}

// ExecWithInput runs a command inside a running container feeding input
// on its stdin, and returns its exit code and combined output. It is
// the credential channel: stdin is the one path into a container that
// Docker persists nowhere — not in the container config, not in an
// image layer, not in the log driver.
func (c *Client) ExecWithInput(ctx context.Context, containerID string, cmd []string, input []byte) (int, string, error) {
	created, err := c.cli.ExecCreate(ctx, containerID, client.ExecCreateOptions{
		Cmd:          cmd,
		AttachStdin:  true,
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

	// Write, then half-close so the process sees EOF; the demux drains
	// until the process exits.
	if _, err := attach.Conn.Write(input); err != nil {
		return -1, "", err
	}
	if err := attach.CloseWrite(); err != nil {
		return -1, "", err
	}
	var buf bytes.Buffer
	if _, err := stdcopy.StdCopy(&buf, &buf, attach.Reader); err != nil {
		return -1, buf.String(), err
	}
	inspect, err := c.cli.ExecInspect(ctx, created.ID, client.ExecInspectOptions{})
	if err != nil {
		return -1, buf.String(), err
	}
	return inspect.ExitCode, buf.String(), nil
}
