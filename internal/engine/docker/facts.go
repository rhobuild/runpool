package docker

import (
	"context"
	"fmt"
	"strings"

	"github.com/moby/moby/client"
)

// DaemonFacts contains Docker-specific host properties that operational and
// qualification tooling may report without exposing Moby types outside this
// adapter. Product decisions continue to consume engine.HostInfo.
type DaemonFacts struct {
	ServerVersion string
	APIVersion    string
	Architecture  string
	CgroupVersion string
	CgroupDriver  string
	StorageDriver string
	DataRoot      string
	Rootless      bool
	Containerd    string
	Runc          string
}

// DaemonFacts reads the Docker properties that identify an execution platform.
func (c *Client) DaemonFacts(ctx context.Context) (DaemonFacts, error) {
	result, err := c.cli.Info(ctx, client.InfoOptions{})
	if err != nil {
		return DaemonFacts{}, fmt.Errorf("read Docker daemon facts: %w", err)
	}
	info := result.Info
	return DaemonFacts{
		ServerVersion: info.ServerVersion,
		APIVersion:    c.cli.ClientVersion(),
		Architecture:  info.Architecture,
		CgroupVersion: info.CgroupVersion,
		CgroupDriver:  info.CgroupDriver,
		StorageDriver: info.Driver,
		DataRoot:      info.DockerRootDir,
		Rootless:      rootlessFromSecurityOptions(info.SecurityOptions),
		Containerd:    info.ContainerdCommit.ID,
		Runc:          info.RuncCommit.ID,
	}, nil
}

func rootlessFromSecurityOptions(options []string) bool {
	for _, option := range options {
		if strings.Contains(option, "rootless") {
			return true
		}
	}
	return false
}
