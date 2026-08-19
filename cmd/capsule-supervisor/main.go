// Command capsule-supervisor is PID 1 of the outer capsule: one
// container holding dockerd, the runner and every inner container the
// job launches, under a single aggregate cgroup. The supervisor is what
// makes that one container honest — it boots the daemon, proves its
// readiness, refuses to let the runner exist before the controller
// authorizes the start, propagates signals, reaps orphans, and reports
// exit status where the controller can read it.
//
// The control surface is the filesystem under /run/runpool, all tmpfs:
//
//	protocol   written at boot, before any state: the protocol version
//	state      booting | waiting | running | exited:<code>
//	           | failed:<reason> | aborted:<reason>
//	           `booting` is the control surface answering before the
//	           daemon is proven; `waiting` is written only once dockerd
//	           answers, because it is the state on which the launcher
//	           delivers a credential and authorizes a start.
//	           `aborted` is a failure before the runner started, so the job
//	           was never handed over and must be retried; `failed` is a
//	           failure after it started, which is an execution outcome.
//	jitconfig  the JIT bundle, delivered by `deliver` over exec stdin,
//	           0600 and owned by the runner uid; consumed at start
//	start      the start authorization: its appearance launches the runner
//
// Subcommands `deliver` and `start` run via docker exec in the same
// container and speak to PID 1 through those files, so the protocol
// needs no network listener and nothing a job inside dind can reach —
// the control directory is outside the runner's uid and the daemon
// socket's group.
package main

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/rhobuild/runpool/internal/capsule/protocol"
)

const (
	// The shared control-surface vocabulary lives in
	// internal/capsule/protocol — one declaration for both sides.
	protocolVersion = protocol.Version
	exitAborted     = protocol.AbortedExitCode
	controlDir      = "/run/runpool"
	stateFile       = controlDir + "/state"
	jitFile         = controlDir + "/jitconfig"
	startFile       = controlDir + "/start"
	protocolFile    = controlDir + "/protocol"

	dockerSocket = "/run/runpool-docker/docker.sock"

	runnerUID  = 1001
	runnerGID  = 1001
	runnerHome = "/home/runner"

	dockerdReadyTimeout = 60 * time.Second
	// dockerdProbeTimeout bounds one readiness probe, so the budget above
	// is spent on repeated asking rather than inside one call that hangs.
	dockerdProbeTimeout = 5 * time.Second
	startPollInterval   = 200 * time.Millisecond
	drainTimeout        = 5 * time.Minute
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "gateway":
			// A long-running mode, not an exec subcommand: the gateway
			// container's PID 1.
			os.Exit(runGateway(log))
		case "gateway-reload":
			os.Exit(runGatewayReload())
		case "gateway-deny-all":
			os.Exit(runGatewayDenyAll())
		}
		os.Exit(runSubcommand(os.Args[1:]))
	}
	os.Exit(supervise(log))
}

// runSubcommand is the exec-side of the protocol, running in the same
// container as PID 1.
func runSubcommand(args []string) int {
	switch args[0] {
	case "deliver":
		// The bundle arrives on stdin and lands on tmpfs, 0600 and
		// runner-owned. This delivery step never places it in Docker
		// metadata or logs it.
		payload, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
		if err != nil || len(payload) == 0 {
			fmt.Fprintln(os.Stderr, "deliver: no credential on stdin")
			return 1
		}
		if err := os.WriteFile(jitFile, payload, 0o600); err != nil {
			fmt.Fprintln(os.Stderr, "deliver:", err)
			return 1
		}
		if err := os.Chown(jitFile, runnerUID, runnerGID); err != nil {
			fmt.Fprintln(os.Stderr, "deliver:", err)
			return 1
		}
		return 0
	case "start":
		if err := os.WriteFile(startFile, []byte(protocolVersion), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, "start:", err)
			return 1
		}
		return 0
	case "state":
		body, err := os.ReadFile(stateFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "state:", err)
			return 1
		}
		fmt.Print(string(body))
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", args[0])
		return 2
	}
}

func supervise(log *slog.Logger) int {
	if err := boot(log); err != nil {
		log.Error("capsule boot failed", "error", err)
		// Boot is entirely before the runner: nothing was ever handed over.
		setState(protocol.AbortedPrefix + err.Error())
		return exitAborted
	}

	code, err := run(log)
	if err != nil {
		log.Error("capsule run failed", "error", err)
		state := terminalFailure(currentState(), err)
		setState(state)
		if strings.HasPrefix(state, protocol.AbortedPrefix) {
			return exitAborted
		}
		return 1
	}
	reported := runnerExit(code)
	if reported != code {
		log.Warn("the runner exited with the reserved abort code; reporting it as a plain failure",
			"runner_exit", code, "reported", reported)
	}
	setState(protocol.ExitedPrefix + strconv.Itoa(reported))
	log.Info("capsule finished", "exit", reported)
	return reported
}

// runnerExit keeps a runner's own status out of the reserved code.
//
// exitAborted means "the runner never started", and it is the only thing
// a stopped capsule can still say — the state file dies with the
// container. But this process also returns the runner's status verbatim,
// so a job that happened to exit with that number would be read as one
// that never ran, requeued, and executed a second time. That is the
// double execution every disposition rule here exists to prevent, and
// reserving a value out of a range the runner also draws from is not
// something to hope about.
//
// Downstream the exact status is only ever logged — the controller
// records an observed exit for any code but the reserved one — so 1 says
// the same thing about a job that ran and did not succeed. The true
// status is logged above before it is replaced.
func runnerExit(code int) int {
	if code == exitAborted {
		return 1
	}
	return code
}

// boot prepares the control surface and the filesystem chores that used
// to need helper containers: with one container there is nobody to
// coordinate with, so root fixes ownership directly before anything
// runs as the runner uid.
func boot(log *slog.Logger) error {
	for _, dir := range []string{controlDir, filepath.Dir(dockerSocket)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	if err := os.WriteFile(protocolFile, []byte(protocolVersion), 0o644); err != nil {
		return err
	}
	// Booting, not waiting: the control surface answers from here, but
	// the daemon the job needs has not been started, let alone proven.
	// Waiting is what the launcher delivers a credential on, and it is
	// written in run() once dockerd answers.
	setState(protocol.StateBooting)
	// The cache lane, when mounted, must be writable by the runner uid.
	if info, err := os.Stat("/cache"); err == nil && info.IsDir() {
		if err := os.Chown("/cache", runnerUID, runnerGID); err != nil {
			return fmt.Errorf("chown cache lane: %w", err)
		}
	}
	wrote, err := writeDockerProxyConfig(os.Getenv, runnerHome, runnerUID, runnerGID)
	if err != nil {
		return err
	}
	if wrote {
		log.Info("job containers take the capsule's proxy")
	}
	log.Info("capsule control surface ready", "protocol", protocolVersion)
	return nil
}

// writeDockerProxyConfig hands the job's own Docker client the proxy the
// capsule was given.
//
// The capsule's environment reaches the runner, its steps and the inner
// daemon, so a checkout, a package install and an image pull all take the
// relay. A container the job starts does not: the daemon creates it, and
// a daemon does not pass its own environment to what it runs. Under the
// restricted profile that container has no route out either, so a
// `docker build` whose Dockerfile fetches anything, or a `docker run`
// that curls, fails with nothing to point at.
//
// The client reads proxies.default from this file and injects it into
// what it builds and runs, which is the one place the setting can be made
// once for every container the job starts. Written unconditionally when
// a proxy exists, before the runner is ever handed a job.
func writeDockerProxyConfig(environ func(string) string, home string, uid, gid int) (bool, error) {
	proxy := environ("HTTP_PROXY")
	if proxy == "" {
		// No relay: the profile is open egress, and a client configured
		// to reach a proxy that does not exist would break what works.
		return false, nil
	}
	cfg := map[string]any{"proxies": map[string]any{"default": map[string]string{
		"httpProxy":  proxy,
		"httpsProxy": cmp.Or(environ("HTTPS_PROXY"), proxy),
		"noProxy":    environ("NO_PROXY"),
	}}}
	body, err := json.Marshal(cfg)
	if err != nil {
		return false, fmt.Errorf("encode docker proxy config: %w", err)
	}
	dir := filepath.Join(home, ".docker")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, fmt.Errorf("create %s: %w", dir, err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return false, fmt.Errorf("write %s: %w", path, err)
	}
	for _, target := range []string{dir, path} {
		if err := os.Chown(target, uid, gid); err != nil {
			return false, fmt.Errorf("chown %s: %w", target, err)
		}
	}
	return true, nil
}

// reaper owns every wait in the capsule. PID 1 must reap orphans that
// reparent to it — dind shims, runner leftovers — but a naive
// wait4(-1) loop races the direct children's own Wait calls and steals
// their exit statuses. So there is exactly one place that calls wait4,
// and children start under the reaper's own lock: a child that dies
// instantly cannot be reaped before it is tracked, because the reap
// loop waits on the same mutex the registration holds. Tracked children
// get their status over a channel; everything else is an orphan, reaped
// and dropped.
type reaper struct {
	mu      sync.Mutex
	tracked map[int]chan int
	sigchld chan os.Signal
}

func newReaper() *reaper {
	r := &reaper{tracked: map[int]chan int{}, sigchld: make(chan os.Signal, 16)}
	signal.Notify(r.sigchld, syscall.SIGCHLD)
	go r.loop()
	return r
}

// start launches a child and registers it atomically with respect to
// the reap loop. The returned channel yields the exit code exactly once.
func (r *reaper) start(cmd *exec.Cmd) (chan int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	ch := make(chan int, 1)
	r.tracked[cmd.Process.Pid] = ch
	return ch, nil
}

func (r *reaper) loop() {
	for range r.sigchld {
		for {
			var status syscall.WaitStatus
			pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
			if pid <= 0 || err != nil {
				break
			}
			code := status.ExitStatus()
			if status.Signaled() {
				code = 128 + int(status.Signal())
			}
			r.mu.Lock()
			if ch, ok := r.tracked[pid]; ok {
				ch <- code
				delete(r.tracked, pid)
			}
			r.mu.Unlock()
		}
	}
}

func run(log *slog.Logger) (int, error) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	reap := newReaper()

	dockerd, dockerdExit, err := startDockerd(log, reap)
	if err != nil {
		return -1, err
	}
	defer stopDockerd(log, dockerd, dockerdExit)

	if err := awaitDockerdReady(ctx); err != nil {
		return -1, err
	}
	log.Info("dockerd ready", "socket", dockerSocket)
	// Only now: waiting is the state the launcher reads as "this capsule
	// can be given a job", and the daemon that runs it has just answered.
	setState(protocol.StateWaiting)

	if err := awaitStart(ctx); err != nil {
		// Cancellation while waiting is an abort, not a clean exit: the
		// start was never authorized, so the job was never handed over
		// and must run somewhere else. Reported as an error so the one
		// function that decides started-or-not decides this too — a
		// clean exit here left the controller reading a stop during
		// shutdown as a job that ran and finished.
		return -1, fmt.Errorf("shutdown before the start was authorized: %w", err)
	}

	return runRunner(ctx, log, reap)
}

// startDockerd launches the inner daemon on its own socket path — not
// the default, so an inner job that blindly mounts /var/run cannot find
// it by accident — with its group set so the runner uid can reach it.
func startDockerd(log *slog.Logger, reap *reaper) (*exec.Cmd, chan int, error) {
	cmd := exec.Command("dockerd",
		"--host=unix://"+dockerSocket,
		"--group="+strconv.Itoa(runnerGID),
	)
	// dockerd's own logs are diagnostics, not job output; they go to
	// stderr so the structured stream on stdout stays parseable.
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	exit, err := reap.start(cmd)
	if err != nil {
		return nil, nil, fmt.Errorf("start dockerd: %w", err)
	}
	log.Info("dockerd starting", "pid", cmd.Process.Pid)
	return cmd, exit, nil
}

func stopDockerd(log *slog.Logger, dockerd *exec.Cmd, exit chan int) {
	if dockerd.Process == nil {
		return
	}
	_ = dockerd.Process.Signal(syscall.SIGTERM)
	select {
	case <-exit:
	case <-time.After(30 * time.Second):
		log.Warn("dockerd did not stop in time; killing it")
		_ = dockerd.Process.Kill()
		<-exit
	}
}

func awaitDockerdReady(ctx context.Context) error {
	deadline := time.Now().Add(dockerdReadyTimeout)
	for {
		if probeDockerd(ctx) == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("dockerd did not become ready in time")
		}
		select {
		case <-time.After(500 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// probeDockerd asks the inner daemon once, under a bound of its own. A
// daemon that accepts the socket and then never answers would otherwise
// spend the whole readiness budget inside a single call, and PID 1 has
// nobody outside it to cut that short.
func probeDockerd(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, dockerdProbeTimeout)
	defer cancel()
	probe := exec.CommandContext(ctx, "docker", "--host=unix://"+dockerSocket, "info")
	// The null device, not a discarding writer. A writer that is not an
	// *os.File makes Cmd build a pipe and copy from it, and Wait then
	// waits for that copy to finish — which a process the kill orphaned
	// keeps open. The probe would return only when the orphan did,
	// leaving the bound above describing nothing.
	probe.Stdout, probe.Stderr = nil, nil
	return probe.Run()
}

// awaitStart blocks until the controller authorizes the start. The
// sentinel is a file because the authorizer runs as a sibling exec in
// this same container: no listener, no port, nothing an inner job can
// reach.
func awaitStart(ctx context.Context) error {
	for {
		if _, err := os.Stat(startFile); err == nil {
			return nil
		}
		select {
		case <-time.After(startPollInterval):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// runRunner launches the runner as the runner uid and waits it out. The JIT
// bundle arrives through tmpfs and the configuration files it instructs the
// upstream runner to materialize are redirected back onto that tmpfs. GitHub's
// runner contract requires the encoded bundle in --jitconfig; it therefore
// exists transiently in the runner's argv, but never in Docker metadata, the
// controller store, the container environment, or the capsule's disk-backed
// writable layer.
func runRunner(ctx context.Context, log *slog.Logger, reap *reaper) (int, error) {
	payload, err := os.ReadFile(jitFile)
	if err != nil {
		return -1, errors.New("start authorized but no credential was delivered")
	}
	if len(payload) == 0 {
		return -1, errors.New("start authorized with an empty credential")
	}
	cleanupConfig, err := prepareRunnerConfig(string(payload), runnerHome,
		filepath.Join(controlDir, "runner-config"), runnerUID, runnerGID)
	if err != nil {
		return -1, fmt.Errorf("prepare volatile runner configuration: %w", err)
	}
	defer cleanupConfig()
	if err := os.Remove(jitFile); err != nil {
		return -1, fmt.Errorf("unlink delivered credential: %w", err)
	}
	log.Info("runner starting")

	cmd := exec.Command(filepath.Join(runnerHome, "run.sh"), "--jitconfig", string(payload))
	cmd.Dir = runnerHome
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(),
		"DOCKER_HOST=unix://"+dockerSocket,
		"HOME="+runnerHome,
		"USER=runner",
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: runnerUID, Gid: runnerGID},
	}
	exit, err := reap.start(cmd)
	// Drop the supervisor's references once fork/exec has copied argv. The
	// child retains the value because the upstream interface requires it.
	cmd.Args = nil
	for i := range payload {
		payload[i] = 0
	}
	if err != nil {
		// fork/exec failed, so the runner never existed and the job was
		// never handed over. `running` is written below, after this check,
		// precisely so this case stays an abort.
		return -1, fmt.Errorf("start runner: %w", err)
	}
	setState(protocol.StateRunning)

	select {
	case code := <-exit:
		return code, nil
	case <-ctx.Done():
		// Graceful drain: the runner finishes its current job on
		// SIGTERM; past the drain window it is killed and the capsule
		// reports the interruption.
		log.Info("draining the runner", "timeout", drainTimeout.String())
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case code := <-exit:
			return code, nil
		case <-time.After(drainTimeout):
			log.Warn("runner did not drain in time; killing it")
			_ = cmd.Process.Kill()
			<-exit
			return -1, errors.New("runner killed after drain timeout")
		}
	}
}

// prepareRunnerConfig redirects every file named by the encoded JIT bundle to
// volatile storage. The upstream runner decodes the same map and writes each
// value beneath its installation root; pre-creating symlinks lets that write
// follow the reviewed path while keeping credentials off the container layer.
func prepareRunnerConfig(encoded, runnerRoot, volatileRoot string, uid, gid int) (func(), error) {
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("decode JIT bundle: %w", err)
	}
	var files map[string]string
	if err := json.Unmarshal(decoded, &files); err != nil {
		return nil, fmt.Errorf("parse JIT bundle: %w", err)
	}
	if len(files) == 0 {
		return func() {}, nil
	}
	if err := os.MkdirAll(volatileRoot, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chown(volatileRoot, uid, gid); err != nil {
		return nil, err
	}
	links := make([]string, 0, len(files))
	cleanup := func() {
		for _, link := range links {
			_ = os.Remove(link)
		}
		_ = os.RemoveAll(volatileRoot)
	}
	for name, content := range files {
		clean := filepath.Clean(name)
		if name == "" || clean == "." || clean != name || filepath.IsAbs(name) ||
			filepath.Base(clean) != clean || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			cleanup()
			return nil, fmt.Errorf("JIT bundle names unsafe path %q", name)
		}
		link := filepath.Join(runnerRoot, clean)
		if _, err := os.Lstat(link); err == nil {
			cleanup()
			return nil, fmt.Errorf("runner configuration path %q already exists", clean)
		} else if !errors.Is(err, os.ErrNotExist) {
			cleanup()
			return nil, err
		}
		target := filepath.Join(volatileRoot, clean)
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			cleanup()
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
			cleanup()
			return nil, err
		}
		// The bundle's content is the runner's configuration and it has to
		// land in the file: the runner reads .runner while constructing
		// its host context, before it looks at --jitconfig, and aborts on
		// a file that exists empty. An empty placeholder was enough only
		// for runner versions that never opened these files. Each value
		// is base64 of the file's bytes, one more layer than the bundle
		// itself.
		decoded, err := base64.StdEncoding.DecodeString(content)
		if err != nil {
			cleanup()
			return nil, fmt.Errorf("decode JIT bundle file %q: %w", clean, err)
		}
		if err := os.WriteFile(target, decoded, 0o600); err != nil {
			cleanup()
			return nil, err
		}
		if err := os.Chown(target, uid, gid); err != nil {
			cleanup()
			return nil, err
		}
		if err := os.Symlink(target, link); err != nil {
			cleanup()
			return nil, err
		}
		links = append(links, link)
	}
	return cleanup, nil
}

func setState(s string) {
	_ = os.WriteFile(stateFile, []byte(s), 0o644)
}

// terminalFailure names a failure by whether the runner ever started, which
// is the one thing the controller cannot infer from the outside. `running` is
// written immediately before the runner is executed, so its presence is the
// proof that the job was handed over: after it, a failure is an execution
// outcome. Before it — no credential delivered, configuration unprepared,
// dockerd never ready — the job never ran, and reporting an exit would settle
// it as complete and never retry it.
func terminalFailure(state string, err error) string {
	if state == protocol.StateRunning {
		return protocol.FailedPrefix + err.Error()
	}
	return protocol.AbortedPrefix + err.Error()
}

func currentState() string {
	body, err := os.ReadFile(stateFile)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(body))
}
