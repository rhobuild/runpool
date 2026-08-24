package docker

import (
	"context"
	"fmt"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"

	"github.com/rhobuild/runpool/internal/assignment"
	"github.com/rhobuild/runpool/internal/engine"
)

// CreateVolume creates a named, labeled volume — an ephemeral capsule
// volume (dind data), removed with its lease.
//
// It inspects before creating, because the daemon's volume create is the
// one create behind this port that does not refuse a taken name: it
// returns the volume that is already there, keeping its original labels,
// and reports no error. Every other create here reports a collision, and
// the callers are built on that report — a taken name is what sends them
// to prove ownership before adopting anything. Without this, a volume
// another owner holds is confirmed as the lease's own and mounted as the
// data root of a privileged daemon.
//
// What it does not close is the instant between the two calls. A volume
// created there is still adopted silently, which is why ownership is
// proven by the caller rather than inferred from this call succeeding.
func (c *Client) CreateVolume(ctx context.Context, name string, labels map[string]string) (string, error) {
	switch _, err := c.cli.VolumeInspect(ctx, name, client.VolumeInspectOptions{}); {
	case err == nil:
		return "", fmt.Errorf("%w: volume %q", engine.ErrAlreadyExists, name)
	case !cerrdefs.IsNotFound(err):
		return "", classify(err)
	}
	created, err := c.cli.VolumeCreate(ctx, client.VolumeCreateOptions{Name: name, Labels: labels})
	if err != nil {
		return "", classify(err)
	}
	return created.Volume.Name, nil
}

// EnsureOwnedVolume makes a persistent volume exist under this owner's
// labels, fail-closed on anyone else's. Docker's volume create silently
// returns an existing volume whatever its labels say, so existence is
// checked first and ownership is proven before the name is reused: a
// foreign volume with the expected name must never be handed to a job
// as its cache.
func (c *Client) EnsureOwnedVolume(ctx context.Context, name string, labels map[string]string) error {
	inspected, err := c.cli.VolumeInspect(ctx, name, client.VolumeInspectOptions{})
	if cerrdefs.IsNotFound(err) {
		_, err := c.cli.VolumeCreate(ctx, client.VolumeCreateOptions{Name: name, Labels: labels})
		return err
	}
	if err != nil {
		return err
	}
	got := inspected.Volume.Labels
	for k, want := range labels {
		if got[k] != want {
			return fmt.Errorf("%w: volume %q (label %s is %q, expected %q)",
				engine.ErrForeignResource, name, k, got[k], want)
		}
	}
	return nil
}

// RemoveVolume removes a volume; one that is already gone is success.
func (c *Client) RemoveVolume(ctx context.Context, name string) error {
	_, err := c.cli.VolumeRemove(ctx, name, client.VolumeRemoveOptions{Force: true})
	return ignoreNotFound(err)
}

func (c *Client) ListOwnedVolumes(ctx context.Context, instanceID assignment.InstanceID) ([]engine.OwnedResource, error) {
	resp, err := c.cli.VolumeList(ctx, client.VolumeListOptions{Filters: ownedFilter(instanceID)})
	if err != nil {
		return nil, err
	}
	out := make([]engine.OwnedResource, 0, len(resp.Items))
	for _, v := range resp.Items {
		own, _ := engine.OwnershipFrom(v.Labels)
		out = append(out, engine.OwnedResource{ID: v.Name, LeaseID: own.Lease, Role: own.Role})
	}
	return out, nil
}
