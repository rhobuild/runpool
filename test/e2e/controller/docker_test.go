package controllere2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type dockerHarness struct {
	settings          settings
	dir               string
	config            string
	secrets           string
	state             string
	control           string
	sentinelNetwork   string
	sentinelVolume    string
	sentinelContainer string
	sentinelIDs       map[string]string
	logs              []string
	stateCreated      bool
	secretsCreated    bool
	sentinelCreated   bool
	configReady       bool
}

func newDockerHarness(s settings, dir string) *dockerHarness {
	return &dockerHarness{
		settings:          s,
		dir:               dir,
		config:            filepath.Join(dir, "config.yaml"),
		secrets:           "runpool-e2e-secrets-" + s.runID,
		state:             "runpool-e2e-state-" + s.runID,
		control:           "runpool-e2e-controller-" + s.runID,
		sentinelNetwork:   "runpool-e2e-sentinel-net-" + s.runID,
		sentinelVolume:    "runpool-e2e-sentinel-vol-" + s.runID,
		sentinelContainer: "runpool-e2e-sentinel-container-" + s.runID,
		sentinelIDs:       map[string]string{},
	}
}

func (d *dockerHarness) prepare(ctx context.Context) error {
	if _, err := d.run(ctx, "version"); err != nil {
		return fmt.Errorf("docker daemon: %w", err)
	}
	// The token travels the way production delivers a secret: written by
	// the daemon into a volume, owned by root with owner-only mode. The
	// controller refuses a token readable by group or other, and its
	// container drops every capability, so a file owned by the runner's
	// user is unreadable however it is moded - root-owned 0600 is the one
	// shape that satisfies both.
	if _, err := d.run(ctx, "volume", "create", "--label", "io.runpool.e2e="+d.settings.runID, d.secrets); err != nil {
		return fmt.Errorf("create secrets volume: %w", err)
	}
	d.secretsCreated = true
	write := exec.CommandContext(ctx, "docker", "run", "--rm", "-i",
		"--user", "0", "--entrypoint", "sh",
		"--label", "io.runpool.e2e="+d.settings.runID,
		"--mount", "type=volume,src="+d.secrets+",dst=/s",
		d.settings.capsuleImage,
		"-c", "umask 077 && cat > /s/provider-token")
	write.Stdin = strings.NewReader(d.settings.token)
	if out, err := write.CombinedOutput(); err != nil {
		return fmt.Errorf("write the provider token into the secrets volume: %w (%s)", err, out)
	}
	if _, err := d.run(ctx, "volume", "create", "--label", "io.runpool.e2e="+d.settings.runID, d.state); err != nil {
		return fmt.Errorf("create state volume: %w", err)
	}
	d.stateCreated = true
	// Mark cleanup responsibility before the first sentinel effect so a
	// partially created fixture is still removed when preparation fails.
	d.sentinelCreated = true
	if out, err := d.run(ctx, "network", "create", "--label", "io.runpool.e2e-sentinel="+d.settings.runID, d.sentinelNetwork); err != nil {
		return fmt.Errorf("create sentinel network: %w", err)
	} else {
		d.sentinelIDs["network"] = strings.TrimSpace(out)
	}
	if out, err := d.run(ctx, "volume", "create", "--label", "io.runpool.e2e-sentinel="+d.settings.runID, d.sentinelVolume); err != nil {
		return fmt.Errorf("create sentinel volume: %w", err)
	} else {
		d.sentinelIDs["volume"] = strings.TrimSpace(out)
	}
	if out, err := d.run(ctx, "create", "--name", d.sentinelContainer,
		"--label", "io.runpool.e2e-sentinel="+d.settings.runID,
		"--network", d.sentinelNetwork,
		"--mount", "type=volume,src="+d.sentinelVolume+",dst=/sentinel",
		d.settings.controllerImage, "version"); err != nil {
		return fmt.Errorf("create sentinel container: %w", err)
	} else {
		d.sentinelIDs["container"] = strings.TrimSpace(out)
	}
	return d.verifyControllerPair(ctx)
}

func (d *dockerHarness) verifyControllerPair(ctx context.Context) error {
	out, err := d.run(ctx, "run", "--rm", "--entrypoint", "/usr/local/bin/runpool",
		d.settings.controllerImage, "version", "--json")
	if err != nil {
		return fmt.Errorf("read controller build facts: %w", err)
	}
	var facts struct {
		CapsuleImage string `json:"capsule_image"`
	}
	if err := json.Unmarshal([]byte(out), &facts); err != nil {
		return fmt.Errorf("decode controller build facts: %w", err)
	}
	if facts.CapsuleImage != d.settings.capsuleImage {
		return fmt.Errorf("controller embeds capsule %q; qualification selected %q", facts.CapsuleImage, d.settings.capsuleImage)
	}
	return nil
}

func (d *dockerHarness) start(ctx context.Context, generation string) error {
	if err := os.WriteFile(d.config, []byte(d.configuration(generation)), 0o644); err != nil {
		return err
	}
	d.configReady = true
	args := []string{
		"run", "-d", "--name", d.control,
		"--init", "--read-only", "--stop-timeout", "120",
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges:true",
		"--tmpfs", "/tmp:size=64m,mode=1777",
		"--label", "io.runpool.e2e=" + d.settings.runID,
		"--env", "RUNPOOL_CONFIG_FILE=/run/runpool/config.yaml",
		"--env", "RUNPOOL_STATE_DIR=/var/lib/runpool/state",
		"--mount", "type=bind,src=" + d.settings.dockerSocket + ",dst=/var/run/docker.sock,readonly",
		"--mount", "type=volume,src=" + d.state + ",dst=/var/lib/runpool/state",
		"--mount", "type=bind,src=" + d.config + ",dst=/run/runpool/config.yaml,readonly",
		"--mount", "type=volume,src=" + d.secrets + ",dst=/run/secrets,readonly",
		d.settings.controllerImage,
	}
	if _, err := d.run(ctx, args...); err != nil {
		return fmt.Errorf("start controller: %w", err)
	}
	return d.waitForLogCount(ctx, "session open", 1, 5*time.Minute)
}

func (d *dockerHarness) killAndReplace(ctx context.Context, generation string) error {
	if _, err := d.run(ctx, "kill", "--signal", "KILL", d.control); err != nil {
		return fmt.Errorf("SIGKILL controller: %w", err)
	}
	if logs, err := d.run(ctx, "logs", d.control); err == nil {
		d.logs = append(d.logs, logs)
	}
	if _, err := d.run(ctx, "rm", d.control); err != nil {
		return fmt.Errorf("remove killed controller: %w", err)
	}
	if err := d.start(ctx, generation); err != nil {
		return fmt.Errorf("start successor controller: %w", err)
	}
	if err := d.waitForLogCount(ctx, "adopting running capsule", 1, 5*time.Minute); err != nil {
		return fmt.Errorf("successor did not adopt the live capsule: %w", err)
	}
	return nil
}

func (d *dockerHarness) switchGeneration(ctx context.Context, generation string) error {
	if err := d.stopAndRemove(ctx); err != nil {
		return err
	}
	return d.start(ctx, generation)
}

func (d *dockerHarness) waitForLogCount(ctx context.Context, marker string, want int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := d.run(ctx, "logs", d.control)
		if err == nil && strings.Count(out, marker) >= want {
			return nil
		}
		if running, rerr := d.running(ctx); rerr == nil && !running {
			return fmt.Errorf("controller exited before %q appeared in its logs: %s", marker, out)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("controller did not report occurrence %d of %q within %s", want, marker, timeout)
}

func (d *dockerHarness) running(ctx context.Context) (bool, error) {
	out, err := d.run(ctx, "inspect", "--format", "{{.State.Running}}", d.control)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "true", nil
}

func (d *dockerHarness) stopAndRemove(ctx context.Context) error {
	if out, err := d.run(ctx, "logs", d.control); err == nil {
		d.logs = append(d.logs, out)
	}
	if _, err := d.run(ctx, "stop", "--time", "120", d.control); err != nil && !isMissingDockerObject(err) {
		return fmt.Errorf("stop controller: %w", err)
	}
	if _, err := d.run(ctx, "rm", "-f", d.control); err != nil && !isMissingDockerObject(err) {
		return fmt.Errorf("remove controller: %w", err)
	}
	return nil
}

func (d *dockerHarness) cleanup(ctx context.Context) (map[string]string, error) {
	result := map[string]string{}
	var errs []error
	if err := d.captureSecretChecks(ctx); err != nil {
		errs = append(errs, err)
	} else {
		result["secret_exposure"] = "not observed"
	}
	if err := d.stopAndRemove(ctx); err != nil {
		errs = append(errs, err)
	}

	if d.configReady {
		instance, err := d.instanceID(ctx)
		if err != nil {
			errs = append(errs, err)
		} else if instance != "" {
			if err := d.uninstall(ctx, instance); err != nil {
				errs = append(errs, err)
			} else {
				result["uninstall"] = "complete"
			}
			if err := d.assertNoManagedResources(ctx, instance); err != nil {
				errs = append(errs, err)
			} else {
				result["managed_resources"] = "absent"
			}
		}
	}
	if d.secretsCreated {
		if _, err := d.run(ctx, "volume", "rm", d.secrets); err != nil && !isMissingDockerObject(err) {
			errs = append(errs, fmt.Errorf("remove secrets volume: %w", err))
		}
	}
	if d.stateCreated {
		if _, err := d.run(ctx, "volume", "rm", d.state); err != nil && !isMissingDockerObject(err) {
			errs = append(errs, fmt.Errorf("remove state volume: %w", err))
		}
	}
	if d.sentinelCreated {
		checks := map[string]string{
			"container": d.sentinelContainer,
			"network":   d.sentinelNetwork,
			"volume":    d.sentinelVolume,
		}
		preserved := true
		for kind, name := range checks {
			var out string
			var err error
			switch kind {
			case "container":
				out, err = d.run(ctx, "inspect", "--format", "{{.Id}}", name)
			case "network":
				out, err = d.run(ctx, "network", "inspect", "--format", "{{.Id}}", name)
			case "volume":
				out, err = d.run(ctx, "volume", "inspect", "--format", "{{.Name}}", name)
			}
			if err != nil {
				preserved = false
				errs = append(errs, fmt.Errorf("inspect foreign sentinel %s: %w", kind, err))
				continue
			}
			if id := strings.TrimSpace(out); id != d.sentinelIDs[kind] {
				preserved = false
				errs = append(errs, fmt.Errorf("foreign sentinel %s changed id: got %q, want %q",
					kind, id, d.sentinelIDs[kind]))
			}
		}
		if preserved {
			result["foreign_sentinels"] = "container, network and volume preserved"
		}
		if _, err := d.run(ctx, "rm", "-f", d.sentinelContainer); err != nil && !isMissingDockerObject(err) {
			errs = append(errs, fmt.Errorf("remove sentinel container: %w", err))
		}
		if _, err := d.run(ctx, "network", "rm", d.sentinelNetwork); err != nil && !isMissingDockerObject(err) {
			errs = append(errs, fmt.Errorf("remove sentinel network: %w", err))
		}
		if _, err := d.run(ctx, "volume", "rm", d.sentinelVolume); err != nil && !isMissingDockerObject(err) {
			errs = append(errs, fmt.Errorf("remove sentinel volume: %w", err))
		}
	}
	return result, errors.Join(errs...)
}

func (d *dockerHarness) captureSecretChecks(ctx context.Context) error {
	if out, err := d.run(ctx, "logs", d.control); err == nil {
		d.logs = append(d.logs, out)
	}
	for _, logs := range d.logs {
		if strings.Contains(logs, d.settings.token) {
			return errors.New("provider token appears in controller logs")
		}
	}
	out, err := d.run(ctx, "inspect", "--format", "{{json .Config.Env}}", d.control)
	if err != nil && !isMissingDockerObject(err) {
		return fmt.Errorf("inspect controller environment: %w", err)
	}
	if strings.Contains(out, d.settings.token) {
		return errors.New("provider token appears in controller environment")
	}
	return nil
}

func (d *dockerHarness) instanceID(ctx context.Context) (string, error) {
	out, err := d.runOneOff(ctx, "status", "--json")
	if err != nil {
		if strings.Contains(err.Error(), "no state") {
			return "", nil
		}
		return "", fmt.Errorf("read final status: %w", err)
	}
	var status struct {
		Instance string `json:"instance"`
	}
	if err := json.Unmarshal([]byte(out), &status); err != nil {
		return "", fmt.Errorf("decode final status: %w (output: %q)", err, out)
	}
	return status.Instance, nil
}

func (d *dockerHarness) uninstall(ctx context.Context, instance string) error {
	_, err := d.runOneOff(ctx, "uninstall", "--confirm="+instance, "--delete-scale-sets")
	if err != nil {
		return fmt.Errorf("uninstall instance %s: %w", instance, err)
	}
	return nil
}

func (d *dockerHarness) runOneOff(ctx context.Context, command ...string) (string, error) {
	args := []string{
		"run", "--rm",
		"--env", "RUNPOOL_CONFIG_FILE=/run/runpool/config.yaml",
		"--env", "RUNPOOL_STATE_DIR=/var/lib/runpool/state",
		"--mount", "type=bind,src=" + d.settings.dockerSocket + ",dst=/var/run/docker.sock,readonly",
		"--mount", "type=volume,src=" + d.state + ",dst=/var/lib/runpool/state",
		"--mount", "type=bind,src=" + d.config + ",dst=/run/runpool/config.yaml,readonly",
		"--mount", "type=volume,src=" + d.secrets + ",dst=/run/secrets,readonly",
		d.settings.controllerImage,
	}
	return d.run(ctx, append(args, command...)...)
}

func (d *dockerHarness) assertNoManagedResources(ctx context.Context, instance string) error {
	queries := [][]string{
		{"ps", "-aq", "--filter", "label=io.runpool.managed=true", "--filter", "label=io.runpool.instance=" + instance},
		{"network", "ls", "-q", "--filter", "label=io.runpool.managed=true", "--filter", "label=io.runpool.instance=" + instance},
		{"volume", "ls", "-q", "--filter", "label=io.runpool.managed=true", "--filter", "label=io.runpool.instance=" + instance},
	}
	for _, query := range queries {
		out, err := d.run(ctx, query...)
		if err != nil {
			return err
		}
		if strings.TrimSpace(out) != "" {
			return fmt.Errorf("managed resources remain after uninstall: %s", strings.TrimSpace(out))
		}
	}
	return nil
}

func (d *dockerHarness) configuration(generation string) string {
	return fmt.Sprintf(`apiVersion: runpool.rhobuild.com/v1
kind: RunpoolConfig
instance:
  name: e2e-%s
host:
  topology: shared-daemon
  reserve:
    cpu: "1"
    memory: 2GiB
    swap: 0B
    freeDisk: 20GiB

scheduling:
  parallelism: 1
targets:
  - id: fixture
    url: https://github.com/%s
    credential: fixture
    cache:
      enabled: true
      generation: %s
    tiers:
      - tier: e2e
        scaleSetName: %s
credentials:
  - id: fixture
    type: token
    tokenFile: /run/secrets/provider-token
tiers:
  - id: e2e
    parallelism: 1
    resources:
      cpu: "2"
      memory: 4GiB
      swap: 0B
      pids: 1024
network:
  profile: public-internet-only
  ipv6: disabled
  dns:
    mode: gateway
`, d.settings.runID, d.settings.repository, generation, d.settings.runnerLabel)
}

func (d *dockerHarness) run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("docker %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func isMissingDockerObject(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "no such container") ||
		strings.Contains(message, "no such network") ||
		strings.Contains(message, "no such volume") ||
		strings.Contains(message, "not found")
}
