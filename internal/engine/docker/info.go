package docker

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/moby/moby/client"
	"github.com/rhobuild/runpool/internal/engine"
)

func (c *Client) Info(ctx context.Context) (engine.HostInfo, error) {
	result, err := c.cli.Info(ctx, client.InfoOptions{})
	if err != nil {
		return engine.HostInfo{}, err
	}
	info := result.Info
	swapTotal, swapErr := localSwapTotal()
	return engine.HostInfo{
		ServerVersion:  info.ServerVersion,
		APIVersion:     c.cli.ClientVersion(),
		Architecture:   info.Architecture,
		OSType:         info.OSType,
		CgroupVersion:  info.CgroupVersion,
		CgroupDriver:   info.CgroupDriver,
		MemoryLimit:    info.MemoryLimit,
		SwapLimit:      info.SwapLimit,
		PidsLimit:      info.PidsLimit,
		Rootless:       rootlessFromSecurityOptions(info.SecurityOptions),
		NCPU:           info.NCPU,
		MemTotalBytes:  info.MemTotal,
		SwapTotalBytes: swapTotal,
		SwapTotalKnown: swapErr == nil,
		Warnings:       info.Warnings,
	}, nil
}

// localSwapTotal reads the Linux host view exposed to the controller. Runpool
// supports a local daemon only, so this is the same kernel the daemon uses.
func localSwapTotal() (int64, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return parseSwapTotal(f)
}

func parseSwapTotal(r io.Reader) (int64, error) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 3 || fields[0] != "SwapTotal:" || fields[2] != "kB" {
			continue
		}
		kib, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parse SwapTotal: %w", err)
		}
		return kib * 1024, nil
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	return 0, errors.New("SwapTotal is absent from /proc/meminfo")
}
