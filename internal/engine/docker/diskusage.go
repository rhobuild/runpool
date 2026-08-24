package docker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/moby/moby/client"

	"github.com/rhobuild/runpool/internal/assignment"
	"github.com/rhobuild/runpool/internal/engine"
)

// OwnedVolumeUsage measures every volume this instance owns. The
// disk-usage endpoint has no label filter, so the daemon's full answer
// is filtered here; sizes come from the daemon's own accounting, which
// is the only view that is correct wherever the controller runs.
func (c *Client) OwnedVolumeUsage(ctx context.Context, instanceID assignment.InstanceID) ([]engine.VolumeUsage, error) {
	du, err := c.cli.DiskUsage(ctx, client.DiskUsageOptions{Volumes: true, Verbose: true})
	if err != nil {
		return nil, err
	}
	var out []engine.VolumeUsage
	for _, v := range du.Volumes.Items {
		own, managed := engine.OwnershipFrom(v.Labels)
		if !managed || own.Instance != instanceID {
			continue
		}
		size := int64(-1)
		if v.UsageData != nil {
			size = v.UsageData.Size
		}
		out = append(out, engine.VolumeUsage{
			Name:   v.Name,
			Role:   own.Role,
			Labels: v.Labels,
			Size:   size,
		})
	}
	return out, nil
}

// ProbeFilesystemFree runs the probe. The image is pinned by the caller
// like every image the product runs; the probe container is labeled,
// removed on exit, and mounts nothing. The name carries a nonce so a
// monitor pass and a doctor run never collide on the daemon.
func (c *Client) ProbeFilesystemFree(ctx context.Context, image string, instanceID assignment.InstanceID) (engine.FilesystemFree, error) {
	nonce := make([]byte, 4)
	if _, err := rand.Read(nonce); err != nil {
		return engine.FilesystemFree{}, err
	}
	code, out, err := c.RunTask(ctx, engine.ContainerSpec{
		Name:  "runpool-" + string(instanceID) + "-df-probe-" + hex.EncodeToString(nonce),
		Image: image,
		// The entrypoint is overridden so any pinned image works as the
		// probe — production reuses the capsule image, whose entrypoint
		// is the supervisor.
		Entrypoint: []string{"/bin/sh", "-c"},
		Cmd:        []string{"df -Pk / && df -Pi /"},
		Labels: engine.Ownership{
			Instance: instanceID,
			Kind:     engine.KindContainer,
			Role:     "probe",
		}.Labels(),
	})
	if err != nil {
		return engine.FilesystemFree{}, err
	}
	if code != 0 {
		return engine.FilesystemFree{}, fmt.Errorf("df probe exited %d: %s", code, out)
	}
	return parseDFProbe(out)
}

// parseDFProbe reads the two POSIX-format df tables the probe prints:
// 1024-byte blocks first, inodes second. POSIX format guarantees the
// column layout, which is why the probe insists on -P.
func parseDFProbe(out string) (engine.FilesystemFree, error) {
	var free engine.FilesystemFree
	free.FreeInodes = -1
	table := 0 // 1 = blocks, 2 = inodes
	blocksSeen := false
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 6 {
			continue
		}
		switch fields[1] {
		case "1024-blocks", "1K-blocks":
			table = 1
			continue
		case "Inodes":
			table = 2
			continue
		}
		switch table {
		case 1:
			avail, err := strconv.ParseInt(fields[3], 10, 64)
			if err != nil {
				return free, fmt.Errorf("df blocks row %q: %w", line, err)
			}
			free.FreeBytes = avail * 1024
			blocksSeen = true
			table = 0
		case 2:
			total, err1 := strconv.ParseInt(fields[1], 10, 64)
			avail, err2 := strconv.ParseInt(fields[3], 10, 64)
			if err1 != nil || err2 != nil {
				return free, fmt.Errorf("df inodes row %q: %v %v", line, err1, err2)
			}
			// A zero inode total is a filesystem that does not account
			// inodes, not one that ran out.
			if total > 0 {
				free.FreeInodes = avail
			}
			table = 0
		}
	}
	// Whether the table was parsed, never whether the value is zero: zero
	// available blocks is a full filesystem, which is the single
	// measurement the pressure machine exists to catch. Reporting it as a
	// parse error made the monitor keep the level in force — so a host that
	// filled completely never reached an emergency and admission stayed
	// open, indefinitely, because every later probe failed the same way.
	if !blocksSeen {
		return free, fmt.Errorf("df probe output had no blocks table: %q", out)
	}
	return free, nil
}
