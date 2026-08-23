package docker

import (
	"context"
	"fmt"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"

	"github.com/rhobuild/runpool/internal/assignment"
	"github.com/rhobuild/runpool/internal/engine"
)

// CreateNetwork creates a user-defined bridge.
func (c *Client) CreateNetwork(ctx context.Context, spec engine.NetworkSpec) (string, error) {
	opts := client.NetworkCreateOptions{
		Driver:   "bridge",
		Internal: spec.Internal,
		Labels:   spec.Labels,
	}
	if spec.Isolated {
		opts.Options = map[string]string{
			"com.docker.network.bridge.gateway_mode_ipv4": "isolated",
			"com.docker.network.bridge.gateway_mode_ipv6": "isolated",
		}
	}
	resp, err := c.cli.NetworkCreate(ctx, spec.Name, opts)
	if err != nil {
		return "", classify(err)
	}
	return resp.ID, nil
}

// ConnectNetwork attaches a created container to a second network — the
// gateway's uplink leg; its first leg comes with the container.
func (c *Client) ConnectNetwork(ctx context.Context, networkID, containerID string) error {
	_, err := c.cli.NetworkConnect(ctx, networkID, client.NetworkConnectOptions{Container: containerID})
	return err
}

// ContainerIPOn returns a container's IPv4 address and the subnet
// prefix on one attached network, resolved by inspecting the container:
// the gateway's internal address is what the capsule routes through,
// and the uplink subnet is itself a deny-set entry.
func (c *Client) ContainerIPOn(ctx context.Context, containerID, networkID string) (ip, subnet string, err error) {
	inspected, err := c.cli.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		return "", "", err
	}
	for _, es := range inspected.Container.NetworkSettings.Networks {
		if es.NetworkID != networkID {
			continue
		}
		if !es.IPAddress.IsValid() {
			return "", "", fmt.Errorf("container %s has no IPv4 address on network %s", engine.ShortID(containerID), engine.ShortID(networkID))
		}
		prefix, err := es.IPAddress.Prefix(es.IPPrefixLen)
		if err != nil {
			return "", "", err
		}
		return es.IPAddress.String(), prefix.Masked().String(), nil
	}
	return "", "", fmt.Errorf("container %s is not attached to network %s", engine.ShortID(containerID), engine.ShortID(networkID))
}

// RemoveNetwork removes a network; one that is already gone is success.
func (c *Client) RemoveNetwork(ctx context.Context, id string) error {
	_, err := c.cli.NetworkRemove(ctx, id, client.NetworkRemoveOptions{})
	return ignoreNotFound(err)
}

// EnsureOwnedNetwork makes a persistent network exist under this
// owner's labels, fail-closed on anyone else's — the uplink's
// counterpart to EnsureOwnedVolume. It returns the network id.
func (c *Client) EnsureOwnedNetwork(ctx context.Context, spec engine.NetworkSpec) (string, error) {
	inspected, err := c.cli.NetworkInspect(ctx, spec.Name, client.NetworkInspectOptions{})
	if cerrdefs.IsNotFound(err) {
		return c.CreateNetwork(ctx, spec)
	}
	if err != nil {
		return "", err
	}
	got := inspected.Network.Labels
	for k, want := range spec.Labels {
		if got[k] != want {
			return "", fmt.Errorf("%w: network %q (label %s is %q, expected %q)",
				engine.ErrForeignResource, spec.Name, k, got[k], want)
		}
	}
	return inspected.Network.ID, nil
}

// NetworkSubnet returns a network's IPv4 subnet as assigned by IPAM.
func (c *Client) NetworkSubnet(ctx context.Context, id string) (string, error) {
	inspected, err := c.cli.NetworkInspect(ctx, id, client.NetworkInspectOptions{})
	if err != nil {
		return "", err
	}
	for _, cfg := range inspected.Network.IPAM.Config {
		if cfg.Subnet.IsValid() && cfg.Subnet.Addr().Is4() {
			return cfg.Subnet.String(), nil
		}
	}
	return "", fmt.Errorf("network %s has no IPv4 subnet", engine.ShortID(id))
}

// AllNetworkSubnets lists every subnet the daemon knows, whoever owns
// the network — part of the deny-set snapshot: another network on this
// daemon is never a legitimate capsule destination.
func (c *Client) AllNetworkSubnets(ctx context.Context) ([]string, error) {
	nets, err := c.cli.NetworkList(ctx, client.NetworkListOptions{})
	if err != nil {
		return nil, err
	}
	var out []string
	for _, n := range nets.Items {
		inspected, err := c.cli.NetworkInspect(ctx, n.ID, client.NetworkInspectOptions{})
		if err != nil {
			if cerrdefs.IsNotFound(err) {
				continue // removed between list and inspect
			}
			return nil, err
		}
		for _, cfg := range inspected.Network.IPAM.Config {
			if cfg.Subnet.IsValid() {
				out = append(out, cfg.Subnet.String())
			}
		}
	}
	return out, nil
}

func (c *Client) ListOwnedNetworks(ctx context.Context, instanceID assignment.InstanceID) ([]engine.OwnedResource, error) {
	nets, err := c.cli.NetworkList(ctx, client.NetworkListOptions{Filters: ownedFilter(instanceID)})
	if err != nil {
		return nil, err
	}
	out := make([]engine.OwnedResource, 0, len(nets.Items))
	for _, n := range nets.Items {
		own, _ := engine.OwnershipFrom(n.Labels)
		out = append(out, engine.OwnedResource{ID: n.ID, LeaseID: own.Lease, Role: own.Role})
	}
	return out, nil
}
