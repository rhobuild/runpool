// Package hostfacts collects the exact host properties attached to release
// qualification evidence. Collection reports what is installed; the platform
// package separately decides whether those facts match a reviewed reference.
package hostfacts

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	enginedocker "github.com/rhobuild/runpool/internal/engine/docker"
	"github.com/rhobuild/runpool/internal/platform"
)

// Document is the collected fact set and the time at which it was observed.
type Document struct {
	platform.Facts
	CollectedAt string `json:"collected_at"`
}

type dockerFacts struct {
	engine        string
	api           string
	cgroupVersion string
	cgroupDriver  string
	storageDriver string
	dockerRoot    string
	rootless      bool
	containerd    string
	runc          string
}

type collector struct {
	readFile  func(string) ([]byte, error)
	run       func(context.Context, string, ...string) (string, error)
	docker    func(context.Context) (dockerFacts, error)
	now       func() time.Time
	goarch    string
	osRelease string
}

// Collect observes the local Linux host and Docker daemon.
func Collect(ctx context.Context) (Document, error) {
	c := collector{
		readFile:  os.ReadFile,
		run:       runCommand,
		docker:    collectDockerFacts,
		now:       time.Now,
		goarch:    runtime.GOARCH,
		osRelease: "/etc/os-release",
	}
	return c.collect(ctx)
}

func (c collector) collect(ctx context.Context) (Document, error) {
	releaseBody, err := c.readFile(c.osRelease)
	if err != nil {
		return Document{}, fmt.Errorf("read operating-system release: %w", err)
	}
	release, err := parseOSRelease(strings.NewReader(string(releaseBody)))
	if err != nil {
		return Document{}, fmt.Errorf("parse operating-system release: %w", err)
	}

	docker, err := c.docker(ctx)
	if err != nil {
		return Document{}, err
	}
	kernel, err := c.run(ctx, "uname", "-r")
	if err != nil {
		return Document{}, fmt.Errorf("read kernel release: %w", err)
	}
	backing, err := c.run(ctx, "findmnt", "-no", "FSTYPE", "--target", docker.dockerRoot)
	if err != nil {
		return Document{}, fmt.Errorf("read Docker backing filesystem with findmnt: %w", err)
	}

	rootless := docker.rootless
	return Document{
		Facts: platform.Facts{
			OS:                release["ID"],
			OSVersion:         release["VERSION_ID"],
			OSCodename:        release["VERSION_CODENAME"],
			Arch:              c.goarch,
			Kernel:            strings.TrimSpace(kernel),
			Engine:            docker.engine,
			API:               docker.api,
			CgroupVersion:     docker.cgroupVersion,
			CgroupDriver:      docker.cgroupDriver,
			StorageDriver:     docker.storageDriver,
			BackingFilesystem: strings.TrimSpace(backing),
			Rootless:          &rootless,
			Containerd:        docker.containerd,
			Runc:              docker.runc,
			Buildx:            secondField(c.runOptional(ctx, "docker", "buildx", "version")),
			Compose:           strings.TrimSpace(c.runOptional(ctx, "docker", "compose", "version", "--short")),
			IPTables:          c.netfilterVersion(ctx, "iptables"),
			NFTables:          c.netfilterVersion(ctx, "nft"),
		},
		CollectedAt: c.now().UTC().Format(time.RFC3339),
	}, nil
}

func runCommand(ctx context.Context, name string, arguments ...string) (string, error) {
	output, err := exec.CommandContext(ctx, name, arguments...).CombinedOutput()
	if err != nil {
		command := strings.Join(append([]string{name}, arguments...), " ")
		if detail := strings.TrimSpace(string(output)); detail != "" {
			return "", fmt.Errorf("%s: %w: %s", command, err, detail)
		}
		return "", fmt.Errorf("%s: %w", command, err)
	}
	return string(output), nil
}

func (c collector) runOptional(ctx context.Context, name string, arguments ...string) string {
	output, err := c.run(ctx, name, arguments...)
	if err != nil {
		return ""
	}
	return output
}

func (c collector) netfilterVersion(ctx context.Context, name string) string {
	for _, candidate := range []string{name, "/usr/local/sbin/" + name, "/usr/sbin/" + name, "/sbin/" + name} {
		output, err := c.run(ctx, candidate, "--version")
		if err == nil {
			return strings.TrimSpace(output)
		}
	}
	return ""
}

func secondField(value string) string {
	fields := strings.Fields(value)
	if len(fields) < 2 {
		return ""
	}
	return fields[1]
}

func collectDockerFacts(ctx context.Context) (dockerFacts, error) {
	cli, err := enginedocker.New(ctx)
	if err != nil {
		return dockerFacts{}, err
	}
	defer cli.Close()
	facts, err := cli.DaemonFacts(ctx)
	if err != nil {
		return dockerFacts{}, err
	}
	return dockerFacts{
		engine:        facts.ServerVersion,
		api:           facts.APIVersion,
		cgroupVersion: facts.CgroupVersion,
		cgroupDriver:  facts.CgroupDriver,
		storageDriver: facts.StorageDriver,
		dockerRoot:    facts.DataRoot,
		rootless:      facts.Rootless,
		containerd:    facts.Containerd,
		runc:          facts.Runc,
	}, nil
}

func parseOSRelease(reader io.Reader) (map[string]string, error) {
	values := make(map[string]string)
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, encoded, ok := strings.Cut(line, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("invalid line %q", line)
		}
		value, err := decodeOSReleaseValue(encoded)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		values[key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return values, nil
}

func decodeOSReleaseValue(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if value[0] == '\'' {
		if len(value) < 2 || value[len(value)-1] != '\'' {
			return "", fmt.Errorf("unterminated single-quoted value")
		}
		return value[1 : len(value)-1], nil
	}
	if value[0] != '"' {
		return value, nil
	}
	if len(value) < 2 || value[len(value)-1] != '"' {
		return "", fmt.Errorf("unterminated double-quoted value")
	}
	var decoded strings.Builder
	body := value[1 : len(value)-1]
	for i := 0; i < len(body); i++ {
		if body[i] != '\\' {
			decoded.WriteByte(body[i])
			continue
		}
		if i+1 == len(body) {
			return "", fmt.Errorf("trailing escape")
		}
		i++
		switch body[i] {
		case '$', '`', '"', '\\':
			decoded.WriteByte(body[i])
		default:
			decoded.WriteByte('\\')
			decoded.WriteByte(body[i])
		}
	}
	return decoded.String(), nil
}
