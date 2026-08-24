// Package docker is the daemon mechanics layer: create, await and remove
// owned containers, networks and volumes, run short-lived tasks, exec
// into a container, resolve missing images, and list what this instance
// owns by label. Scheduling policy and capsule orchestration live above
// it.
package docker

import (
	"context"
	"fmt"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

	"github.com/rhobuild/runpool/internal/assignment"
	"github.com/rhobuild/runpool/internal/engine"
)

type Client struct {
	cli *client.Client
	// onCleanupError observes a helper that could not be removed. It is
	// a hook rather than a log call so the caller decides how loud a
	// leaked helper is; the default is silent because the ownership
	// labels guarantee a later sweep finds it.
	onCleanupError func(name string, err error)
}

// OnCleanupError registers an observer for helper removals that failed.
func (c *Client) OnCleanupError(fn func(name string, err error)) { c.onCleanupError = fn }

// New connects to the local daemon and proves liveness immediately: a
// controller that cannot reach its daemon must fail at startup, not at
// first lease.
//
// The ping negotiates the API version rather than leaving it to the
// first real request. Not for speed — serve reads the daemon's facts on
// the next line, so the lazy negotiation was already settled on the
// startup goroutine long before anything ran concurrently. What it buys
// is a failure with somewhere to appear: the lazy path runs inside
// getAPIPath, which discards the error, so a version this client cannot
// use surfaces as whatever the first request happens to fail with. Here
// it surfaces as a refusal to start, next to the reason.
//
// A daemon whose version this client cannot use is one it should not
// start against, and saying so is not the same as saying the daemon is
// unreachable — the daemon answered.
func New(ctx context.Context) (*Client, error) {
	cli, err := client.New(client.FromEnv)
	if err != nil {
		return nil, err
	}
	if _, err := cli.Ping(ctx, client.PingOptions{}); err != nil {
		cli.Close()
		return nil, fmt.Errorf("docker daemon unreachable: %w", err)
	}
	if _, err := cli.Ping(ctx, client.PingOptions{NegotiateAPIVersion: true}); err != nil {
		cli.Close()
		return nil, fmt.Errorf("docker daemon answered but its API version is unusable: %w", err)
	}
	return &Client{cli: cli, onCleanupError: func(string, error) {}}, nil
}

func (c *Client) Close() error { return c.cli.Close() }

func (c *Client) hostConfig(spec engine.ContainerSpec) *container.HostConfig {
	mounts := make([]mount.Mount, len(spec.Mounts))
	for i, m := range spec.Mounts {
		mounts[i] = mount.Mount{Type: mount.TypeVolume, Source: m.Volume, Target: m.Target, ReadOnly: m.ReadOnly}
	}
	networkMode := container.NetworkMode(spec.Network)
	if spec.NetworkMode != "" {
		networkMode = container.NetworkMode(spec.NetworkMode)
	}
	pids := spec.PIDsLimit
	return &container.HostConfig{
		Privileged:  spec.Privileged,
		Tmpfs:       spec.Tmpfs,
		NetworkMode: networkMode,
		DNS:         spec.DNS,
		CapAdd:      spec.CapAdd,
		GroupAdd:    spec.GroupAdd,
		Mounts:      mounts,
		Resources: container.Resources{
			Memory:       spec.MemoryBytes,
			MemorySwap:   spec.MemorySwapBytes,
			NanoCPUs:     spec.NanoCPUs,
			PidsLimit:    &pids,
			CgroupParent: spec.CgroupParent,
		},
	}
}

// CreateContainer creates a container, pulling the image only when the
// daemon does not have it. It does not start it.
func (c *Client) CreateContainer(ctx context.Context, spec engine.ContainerSpec) (string, error) {
	create := func() (string, error) {
		resp, err := c.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
			Config: &container.Config{
				Image:      spec.Image,
				Entrypoint: spec.Entrypoint,
				Cmd:        spec.Cmd,
				Env:        spec.Env,
				User:       spec.User,
				Labels:     spec.Labels,
			},
			HostConfig: c.hostConfig(spec),
			Name:       spec.Name,
		})
		return resp.ID, err
	}
	id, err := create()
	// A missing image is the one create failure worth retrying: pull it
	// and try once more. The typed check avoids depending on the
	// daemon's wording.
	if err != nil && cerrdefs.IsNotFound(err) {
		if err := c.pull(ctx, spec.Image); err != nil {
			return "", err
		}
		id, err = create()
	}
	if err != nil {
		return "", fmt.Errorf("create container %s: %w", spec.Name, classify(err))
	}
	return id, nil
}

func (c *Client) StartContainer(ctx context.Context, id string) error {
	_, err := c.cli.ContainerStart(ctx, id, client.ContainerStartOptions{})
	return err
}

func (c *Client) pull(ctx context.Context, ref string) error {
	resp, err := c.cli.ImagePull(ctx, ref, client.ImagePullOptions{})
	if err != nil {
		return fmt.Errorf("pull %s: %w", ref, err)
	}
	defer resp.Close()
	// Wait drains the progress stream and reports a failure reported
	// inside it, which a plain copy to io.Discard would swallow.
	if err := resp.Wait(ctx); err != nil {
		return fmt.Errorf("pull %s: %w", ref, err)
	}
	return nil
}

// RunTask creates, starts and awaits a short-lived container (a chown or
// externals seed), returning its exit code and combined output, then
// removes it. Tasks are not capsule resources; they clean themselves up.
func (c *Client) RunTask(ctx context.Context, spec engine.ContainerSpec) (int64, string, error) {
	id, err := c.CreateContainer(ctx, spec)
	if err != nil {
		return -1, "", err
	}
	// Removal gets its own deadline: deferring it on the job's context
	// means a cancelled job also cancels its own cleanup, leaving the
	// helper behind until some later restart sweeps it.
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if err := c.RemoveContainer(cleanupCtx, id); err != nil {
			c.onCleanupError(spec.Name, err)
		}
	}()
	if err := c.StartContainer(ctx, id); err != nil {
		return -1, "", err
	}
	code, err := c.WaitExit(ctx, id)
	if err != nil {
		return -1, "", err
	}
	out, _ := c.TailLogs(ctx, id, 50)
	return code, out, nil
}

// WaitExit blocks until the container stops and returns its exit code.
func (c *Client) WaitExit(ctx context.Context, id string) (int64, error) {
	wait := c.cli.ContainerWait(ctx, id, client.ContainerWaitOptions{
		Condition: container.WaitConditionNotRunning,
	})
	select {
	case resp := <-wait.Result:
		if resp.Error != nil {
			return -1, fmt.Errorf("wait on %s: %s", id, resp.Error.Message)
		}
		return resp.StatusCode, nil
	case err := <-wait.Error:
		return -1, err
	}
}

// TailLogs returns the last lines of a container's output — diagnostics
// drained before deletion, never a job-log pipeline.
func (c *Client) TailLogs(ctx context.Context, id string, lines int) (string, error) {
	rd, err := c.cli.ContainerLogs(ctx, id, client.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Tail:       fmt.Sprintf("%d", lines),
	})
	if err != nil {
		return "", err
	}
	defer rd.Close()
	var buf strings.Builder
	if _, err := stdcopy.StdCopy(&buf, &buf, rd); err != nil {
		return buf.String(), err
	}
	return buf.String(), nil
}

// RemoveContainer removes a container. An object that is already gone is
// success: cleanup runs again after crashes, and the goal is absence.
//
// Anonymous volumes go with it. An image that declares VOLUME grows one
// per container wherever a mount does not cover the path, they carry no
// ownership labels, so no sweep ever finds them. Named volumes are not
// touched by this flag.
func (c *Client) RemoveContainer(ctx context.Context, id string) error {
	_, err := c.cli.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
	return ignoreNotFound(err)
}

// ignoreNotFound turns "this does not exist" into success, so a cleanup
// that already partly ran can finish rather than stall. It uses the
// SDK's typed check; matching on message text breaks silently when
// wording changes.
func ignoreNotFound(err error) error {
	if err == nil || cerrdefs.IsNotFound(err) {
		return nil
	}
	return err
}

// classify names what a daemon error means, keeping the daemon's own
// text: the sentinel is for branching and the text is for reading. An
// error it has no name for is returned as it came, because inventing a
// category for it would be a claim this package cannot make.
func classify(err error) error {
	switch {
	case err == nil:
		return nil
	case cerrdefs.IsNotFound(err):
		return fmt.Errorf("%w: %w", engine.ErrNotFound, err)
	case cerrdefs.IsAlreadyExists(err), cerrdefs.IsConflict(err):
		return fmt.Errorf("%w: %w", engine.ErrAlreadyExists, err)
	case cerrdefs.IsUnavailable(err), client.IsErrConnectionFailed(err):
		return fmt.Errorf("%w: %w", engine.ErrUnavailable, err)
	}
	return err
}

// OwnedIDByName resolves an object by its deterministic name and proves
// ownership before returning it. Absence returns ("", nil): for a
// create-side conflict it means the conflict was transient, and for
// recovery it proves the create never took effect. A name match with
// foreign labels is engine.ErrForeignResource — name equality is not ownership.
func (c *Client) OwnedIDByName(ctx context.Context, kind, name string, instanceID assignment.InstanceID, leaseID assignment.LeaseID) (string, error) {
	return c.resolveOwnedID(ctx, kind, name, instanceID, leaseID)
}

// resolveOwnedID accepts either a deterministic name or an immutable daemon
// id. Creation recovery calls it by name; destructive cleanup calls it by the
// confirmed id when one was persisted.
func (c *Client) resolveOwnedID(ctx context.Context, kind, reference string, instanceID assignment.InstanceID, leaseID assignment.LeaseID) (string, error) {
	var id string
	var labels map[string]string
	switch kind {
	case "container":
		inspected, err := c.cli.ContainerInspect(ctx, reference, client.ContainerInspectOptions{})
		if cerrdefs.IsNotFound(err) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		id, labels = inspected.Container.ID, inspected.Container.Config.Labels
	case "network":
		inspected, err := c.cli.NetworkInspect(ctx, reference, client.NetworkInspectOptions{})
		if cerrdefs.IsNotFound(err) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		id, labels = inspected.Network.ID, inspected.Network.Labels
	case "volume":
		inspected, err := c.cli.VolumeInspect(ctx, reference, client.VolumeInspectOptions{})
		if cerrdefs.IsNotFound(err) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		id, labels = inspected.Volume.Name, inspected.Volume.Labels
	default:
		return "", fmt.Errorf("unknown resource kind %q", kind)
	}
	own, managed := engine.OwnershipFrom(labels)
	if !managed || own.Instance != instanceID || own.Lease != leaseID {
		return "", fmt.Errorf("%w: %s %q", engine.ErrForeignResource, kind, reference)
	}
	return id, nil
}

// RemoveOwnedContainer proves the referenced object still belongs to the
// expected instance and lease, then removes the exact inspected id. This is
// the destructive counterpart of OwnedIDByName: a stale intent must never
// delete a foreign object that later reused its deterministic name.
func (c *Client) RemoveOwnedContainer(ctx context.Context, reference string, instanceID assignment.InstanceID, leaseID assignment.LeaseID) error {
	id, err := c.resolveOwnedID(ctx, "container", reference, instanceID, leaseID)
	if err != nil || id == "" {
		return err
	}
	return c.RemoveContainer(ctx, id)
}

func (c *Client) RemoveOwnedNetwork(ctx context.Context, reference string, instanceID assignment.InstanceID, leaseID assignment.LeaseID) error {
	id, err := c.resolveOwnedID(ctx, "network", reference, instanceID, leaseID)
	if err != nil || id == "" {
		return err
	}
	return c.RemoveNetwork(ctx, id)
}

func (c *Client) RemoveOwnedVolume(ctx context.Context, reference string, instanceID assignment.InstanceID, leaseID assignment.LeaseID) error {
	name, err := c.resolveOwnedID(ctx, "volume", reference, instanceID, leaseID)
	if err != nil || name == "" {
		return err
	}
	return c.RemoveVolume(ctx, name)
}

// ContainerCgroupParent reports the parent cgroup a container was
// created under. A lease's capsule and gateway must report the same
// one: that is what makes their limits one aggregate rather than two
// independent budgets.
func (c *Client) ContainerCgroupParent(ctx context.Context, id string) (string, error) {
	inspected, err := c.cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return "", err
	}
	return inspected.Container.HostConfig.CgroupParent, nil
}

// ContainerStatus returns the daemon's state for one container: created,
// running, paused, restarting, removing, exited or dead, with the exit
// code when it has stopped. A not-found error passes through typed so the
// caller can treat absence as an answer rather than a failure.
func (c *Client) ContainerStatus(ctx context.Context, id string) (engine.ContainerState, error) {
	inspected, err := c.cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		// Classified because the answer decides an attempt's fate: a
		// container that is gone ended, and one the daemon cannot be
		// asked about is undecided and goes to a person.
		return engine.ContainerState{}, classify(err)
	}
	if inspected.Container.State == nil {
		return engine.ContainerState{}, fmt.Errorf("container %s reports no state", id)
	}
	return engine.ContainerState{
		Status:   engine.ContainerStatus(inspected.Container.State.Status),
		ExitCode: inspected.Container.State.ExitCode,
	}, nil
}

// ListOwnedContainers finds every container this instance stamped,
// running or not — the reconciliation working set after a crash.
func (c *Client) ListOwnedContainers(ctx context.Context, instanceID assignment.InstanceID) ([]engine.OwnedContainer, error) {
	list, err := c.cli.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: ownedFilter(instanceID),
	})
	if err != nil {
		return nil, err
	}
	out := make([]engine.OwnedContainer, 0, len(list.Items))
	for _, s := range list.Items {
		name := ""
		if len(s.Names) > 0 {
			name = strings.TrimPrefix(s.Names[0], "/")
		}
		// The one conversion: a label is a string until it is read, and
		// this is where it is read.
		own, _ := engine.OwnershipFrom(s.Labels)
		out = append(out, engine.OwnedContainer{
			ID:      s.ID,
			Name:    name,
			Kind:    own.Kind,
			Role:    own.Role,
			LeaseID: own.Lease,
			Running: s.State == container.StateRunning,
		})
	}
	return out, nil
}

func ownedFilter(instanceID assignment.InstanceID) client.Filters {
	filters := client.Filters{}
	for key, value := range engine.ManagedBy(instanceID) {
		filters = filters.Add("label", key+"="+value)
	}
	return filters
}
