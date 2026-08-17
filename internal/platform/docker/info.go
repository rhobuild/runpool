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
)

// HostInfo is what the doctor needs to know about the daemon and its
// kernel: enough to decide whether this host can honour the capsule
// contract, and nothing more.
type HostInfo struct {
	ServerVersion string
	APIVersion    string
	Architecture  string
	OSType        string
	CgroupVersion string
	CgroupDriver  string
	MemoryLimit   bool
	SwapLimit     bool
	PidsLimit     bool
	Rootless      bool
	// Physical capacity as the daemon sees it, for the doctor's
	// tiers-versus-host arithmetic.
	NCPU           int
	MemTotalBytes  int64
	SwapTotalBytes int64
	SwapTotalKnown bool
	Warnings       []string
}

func (c *Client) Info(ctx context.Context) (HostInfo, error) {
	result, err := c.cli.Info(ctx, client.InfoOptions{})
	if err != nil {
		return HostInfo{}, err
	}
	info := result.Info
	// The daemon reports rootless mode as a security option, which is the
	// only portable signal for it.
	rootless := false
	for _, opt := range info.SecurityOptions {
		if strings.Contains(opt, "rootless") {
			rootless = true
		}
	}
	swapTotal, swapErr := localSwapTotal()
	return HostInfo{
		ServerVersion:  info.ServerVersion,
		APIVersion:     c.cli.ClientVersion(),
		Architecture:   info.Architecture,
		OSType:         info.OSType,
		CgroupVersion:  info.CgroupVersion,
		CgroupDriver:   info.CgroupDriver,
		MemoryLimit:    info.MemoryLimit,
		SwapLimit:      info.SwapLimit,
		PidsLimit:      info.PidsLimit,
		Rootless:       rootless,
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
