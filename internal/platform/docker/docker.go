// Package docker is the daemon mechanics layer: create, await and remove
// owned containers, networks and volumes, run short-lived tasks, exec
// into a container, resolve missing images, and list what this instance
// owns by label. Scheduling policy and capsule orchestration live above
// it.
package docker

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

	"github.com/rhobuild/runpool/internal/assignment"
)

// Ownership labels: every resource Runpool creates is identifiable and
// reconcilable through the Docker API alone, so a crashed controller's
// objects can always be found without trusting its own books.
const (
	labelManaged  = "io.runpool.managed"
	labelInstance = "io.runpool.instance"
	labelKind     = "io.runpool.kind"
	labelLease    = "io.runpool.lease"
	labelRole     = "io.runpool.role"
	labelAttempt  = "io.runpool.attempt"
	labelTarget   = "io.runpool.target"
	labelTier     = "io.runpool.tier"
)

// Ownership is what a Runpool instance stamps on everything it creates,
// and the only thing that proves an object is its to remove. A name is
// not proof: a foreign object can carry the name a plan expects, and
// adopting it by name is how a sweep deletes somebody else's work.
//
// It is one value rather than a map each caller builds, because the
// shape is fixed and the six places that built it by hand could each
// forget a key. What varies is which fields a kind of object has:
// instance infrastructure carries no lease, and a probe carries neither
// attempt nor tier. An unset field is left off rather than written
// empty, so an object states what is true of it and nothing else.
type Ownership struct {
	Instance assignment.InstanceID
	Lease    assignment.LeaseID
	Kind     string
	Role     string
	Attempt  string
	Target   string
	Tier     string
}

// Labels renders the ownership as the labels an object carries. The
// values are a compatibility surface between releases: a controller
// sweeps the objects an older one stamped, so a key or a value that
// changes is a sweep that stops finding them. internal/platform/docker's
// own test pins them exactly.
func (o Ownership) Labels() map[string]string {
	labels := map[string]string{
		labelManaged:  "true",
		labelInstance: string(o.Instance),
	}
	for key, value := range map[string]string{
		labelKind:    o.Kind,
		labelLease:   string(o.Lease),
		labelRole:    o.Role,
		labelAttempt: o.Attempt,
		labelTarget:  o.Target,
		labelTier:    o.Tier,
	} {
		if value != "" {
			labels[key] = value
		}
	}
	return labels
}

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

// Mount attaches one named volume into a container at an absolute path,
// whole. Binds and subpaths are deliberately not expressible: every
// mount Runpool makes is a daemon-side volume, which is the same object
// however the controller itself is deployed.
type Mount struct {
	Volume   string
	Target   string
	ReadOnly bool
}

// ContainerSpec is the container shape used by capsules, gateways, and
// short-lived operational probes.
type ContainerSpec struct {
	Name       string
	Image      string
	Entrypoint []string
	Cmd        []string
	Env        []string
	User       string // "" = image default; "0:0" for root setup tasks
	Labels     map[string]string
	Privileged bool
	// NetworkMode overrides Network for daemon-defined modes such as host.
	// Network attaches to a user-defined network by name or id.
	NetworkMode string
	Network     string
	// DNS pins the container's resolvers — the capsule points at its
	// gateway's forwarder, never at anything it could choose itself.
	DNS []netip.Addr
	// CapAdd grants capabilities to an otherwise unprivileged container.
	// The gateway receives NET_ADMIN for its own fail-closed ruleset.
	CapAdd   []string
	GroupAdd []string
	Mounts   []Mount
	// Tmpfs mounts by target path, e.g. "/run/runpool": "rw,size=64k".
	Tmpfs map[string]string

	// Resource limits, the tier envelope applied to the container.
	MemoryBytes     int64
	MemorySwapBytes int64
	NanoCPUs        int64
	PIDsLimit       int64
	// CgroupParent places this container under a shared parent cgroup.
	// A lease's capsule and its gateway name the same parent, so the
	// kernel accounts them as one aggregate and one path reports the
	// lease's whole consumption.
	CgroupParent string
}

func (c *Client) hostConfig(spec ContainerSpec) *container.HostConfig {
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
func (c *Client) CreateContainer(ctx context.Context, spec ContainerSpec) (string, error) {
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
		return "", fmt.Errorf("create container %s: %w", spec.Name, err)
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
func (c *Client) RunTask(ctx context.Context, spec ContainerSpec) (int64, string, error) {
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

// IsNotFound reports whether err means the daemon has no such object.
// Callers that must distinguish absence from unreachability — execution
// inspection, most of all — depend on this being a typed check.
func IsNotFound(err error) bool { return cerrdefs.IsNotFound(err) }

// ErrForeignResource reports an object that carries the expected name
// but not this owner's labels. Adopting it would mean running work
// through — and later deleting — something another instance or a human
// created, so the only safe answer is to stop.
var ErrForeignResource = errors.New("an object with the expected name exists but is not owned by this lease")

// OwnedIDByName resolves an object by its deterministic name and proves
// ownership before returning it. Absence returns ("", nil): for a
// create-side conflict it means the conflict was transient, and for
// recovery it proves the create never took effect. A name match with
// foreign labels is ErrForeignResource — name equality is not ownership.
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
	if labels[labelManaged] != "true" || labels[labelInstance] != string(instanceID) || labels[labelLease] != string(leaseID) {
		return "", fmt.Errorf("%w: %s %q", ErrForeignResource, kind, reference)
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

// ContainerState is what the daemon can still say about a container after
// it has stopped. ExitCode is carried alongside Status because a stopped
// container's own filesystem is no longer reachable — no exec, and its
// tmpfs control surface is gone — so the exit code is the only account of
// itself a capsule can leave behind.
type ContainerState struct {
	Status   string
	ExitCode int
}

// ContainerStatus returns the daemon's state for one container: created,
// running, paused, restarting, removing, exited or dead, with the exit
// code when it has stopped. A not-found error passes through typed so the
// caller can treat absence as an answer rather than a failure.
func (c *Client) ContainerStatus(ctx context.Context, id string) (ContainerState, error) {
	inspected, err := c.cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return ContainerState{}, err
	}
	if inspected.Container.State == nil {
		return ContainerState{}, fmt.Errorf("container %s reports no state", id)
	}
	return ContainerState{
		Status:   string(inspected.Container.State.Status),
		ExitCode: inspected.Container.State.ExitCode,
	}, nil
}

// OwnedContainer is one labeled container found during reconciliation.
type OwnedContainer struct {
	ID      string
	Name    string
	Kind    string
	Role    string
	LeaseID assignment.LeaseID
	Running bool
}

// HelperInFlight reports whether this is a container the instance runs
// for itself and has not finished with — the filesystem probe, host-CIDR
// discovery. A sweep has to leave those alone while they run.
//
// The lease id decides ownership, the same rule
// OwnedResource.InstanceInfrastructure states for networks and volumes.
// The second condition is what makes containers different: a lease-less
// network or volume is persistent, while a helper is ephemeral and
// RunTask removes its own on a deadline of its own. So one still running
// is in flight, and force removing it fails the measurement that started
// it; one that is stopped outlived the process that owned it, and
// collecting that is what a sweep is for. Both conditions live here
// rather than at the call sites because a sweep that tests only the
// first deletes the instance's own working parts.
func (c OwnedContainer) HelperInFlight() bool { return c.LeaseID == "" && c.Running }

// ListOwnedContainers finds every container this instance stamped,
// running or not — the reconciliation working set after a crash.
func (c *Client) ListOwnedContainers(ctx context.Context, instanceID assignment.InstanceID) ([]OwnedContainer, error) {
	list, err := c.cli.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: ownedFilter(instanceID),
	})
	if err != nil {
		return nil, err
	}
	out := make([]OwnedContainer, 0, len(list.Items))
	for _, s := range list.Items {
		name := ""
		if len(s.Names) > 0 {
			name = strings.TrimPrefix(s.Names[0], "/")
		}
		out = append(out, OwnedContainer{
			ID:   s.ID,
			Name: name,
			Kind: s.Labels[labelKind],
			Role: s.Labels[labelRole],
			// The one conversion: a label is a string until it is read,
			// and this is where it is read.
			LeaseID: assignment.LeaseID(s.Labels[labelLease]),
			Running: s.State == container.StateRunning,
		})
	}
	return out, nil
}

func ownedFilter(instanceID assignment.InstanceID) client.Filters {
	return client.Filters{}.
		Add("label", labelManaged+"=true").
		Add("label", labelInstance+"="+string(instanceID))
}
