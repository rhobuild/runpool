package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func encodedBundle(t *testing.T, body string) string {
	t.Helper()
	return base64.StdEncoding.EncodeToString([]byte(body))
}

func TestPrepareRunnerConfigUsesVolatileTargets(t *testing.T) {
	root := t.TempDir()
	runnerRoot := filepath.Join(root, "runner")
	volatileRoot := filepath.Join(root, "control", "runner-config")
	if err := os.MkdirAll(runnerRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	// Each bundle value is base64 of the file's bytes, as the provider
	// encodes it - and the content has to arrive: the runner reads
	// .runner at boot and aborts on a file that exists empty.
	contents := map[string]string{".runner": `{"agentName":"probe"}`, ".credentials": `{"scheme":"OAuth"}`}
	bundle := fmt.Sprintf(`{".runner":%q,".credentials":%q}`,
		base64.StdEncoding.EncodeToString([]byte(contents[".runner"])),
		base64.StdEncoding.EncodeToString([]byte(contents[".credentials"])))
	cleanup, err := prepareRunnerConfig(encodedBundle(t, bundle),
		runnerRoot, volatileRoot, os.Getuid(), os.Getgid())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".runner", ".credentials"} {
		link := filepath.Join(runnerRoot, name)
		info, err := os.Lstat(link)
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("%s is not a symlink: %v, %v", name, info, err)
		}
		body, err := os.ReadFile(link)
		if err != nil || string(body) != contents[name] {
			t.Fatalf("materialized %s = %q, %v; want the bundle's decoded bytes", name, body, err)
		}
		if err := os.WriteFile(link, []byte("secret"), 0o600); err != nil {
			t.Fatal(err)
		}
		body, err = os.ReadFile(filepath.Join(volatileRoot, name))
		if err != nil || string(body) != "secret" {
			t.Fatalf("volatile target %s = %q, %v", name, body, err)
		}
	}
	cleanup()
	if _, err := os.Lstat(filepath.Join(runnerRoot, ".credentials")); !os.IsNotExist(err) {
		t.Fatalf("cleanup left a runner configuration link: %v", err)
	}
	if _, err := os.Stat(volatileRoot); !os.IsNotExist(err) {
		t.Fatalf("cleanup left volatile configuration: %v", err)
	}
}

func TestPrepareRunnerConfigRejectsPathsAndCollisions(t *testing.T) {
	for _, name := range []string{"../escape", "nested/file", "/absolute"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if _, err := prepareRunnerConfig(encodedBundle(t, `{"`+name+`":"ignored"}`),
				root, filepath.Join(root, "volatile"), os.Getuid(), os.Getgid()); err == nil {
				t.Fatalf("unsafe JIT path %q was accepted", name)
			}
		})
	}

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".credentials"), []byte("foreign"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareRunnerConfig(encodedBundle(t, `{".credentials":"ignored"}`),
		root, filepath.Join(root, "volatile"), os.Getuid(), os.Getgid()); err == nil {
		t.Fatal("an existing runner configuration file was replaced")
	}
}

// TestTerminalFailureDistinguishesUnstartedRunners: `running` is written
// immediately before the runner is executed, so it is the only proof the
// supervisor has that the job was handed over. A failure before it must not
// be reported as an execution, or the controller settles a job that never ran.
func TestTerminalFailureDistinguishesUnstartedRunners(t *testing.T) {
	err := errors.New("no credential was delivered")

	for _, state := range []string{"", "waiting"} {
		if got := terminalFailure(state, err); !strings.HasPrefix(got, "aborted:") {
			t.Errorf("failure from state %q = %q; want an aborted state", state, got)
		}
	}
	// Past `running` the runner owned the job; claiming it never started
	// would run it a second time.
	if got := terminalFailure("running", err); !strings.HasPrefix(got, "failed:") {
		t.Errorf("failure after the runner started = %q; want a failed state", got)
	}
}

// TestRunnerStatusNeverCollidesWithTheAbortCode. This process returns the
// runner's own status as the capsule's, and the controller reads
// exitAborted as proof the runner never started — the one account a
// stopped capsule can still give, since the state file dies with the
// container. A job that exits with that number would therefore be
// settled as unstarted and run again, which is the double execution
// every disposition rule here exists to prevent.
//
// Every other status passes through untouched, including zero: the
// controller distinguishes success from failure by the code it is given,
// and only the reserved value may not be one of them.
func TestRunnerStatusNeverCollidesWithTheAbortCode(t *testing.T) {
	for _, code := range []int{0, 1, 2, 78, 80, 125, 126, 127, 130, 255} {
		if got := runnerExit(code); got != code {
			t.Errorf("a runner status of %d was reported as %d; only the reserved code is remapped", code, got)
		}
	}

	got := runnerExit(exitAborted)
	if got == exitAborted {
		t.Fatalf("a runner that exited %d is reported as an abort; its job would be run twice", exitAborted)
	}
	if got == 0 {
		t.Errorf("a runner that exited %d is reported as success; its job failed", exitAborted)
	}
}

// TestWriteDockerProxyConfig: the capsule's environment reaches the
// runner, its steps and the inner daemon, but not the containers that
// daemon creates — a daemon does not pass its own environment to what it
// runs. Under the restricted profile those containers also have no route
// out, so a `docker build` that fetches anything fails with nothing to
// point at. The client reads this file and injects the proxy into every
// container the job starts.
func TestWriteDockerProxyConfig(t *testing.T) {
	uid, gid := os.Getuid(), os.Getgid()

	t.Run("a relay reaches the job's own containers", func(t *testing.T) {
		home := t.TempDir()
		env := map[string]string{
			"HTTP_PROXY":  "http://10.0.0.2:3128",
			"HTTPS_PROXY": "http://10.0.0.2:3128",
			"NO_PROXY":    "localhost,127.0.0.1",
		}
		wrote, err := writeDockerProxyConfig(func(k string) string { return env[k] }, home, uid, gid)
		if err != nil {
			t.Fatal(err)
		}
		if !wrote {
			t.Fatal("no config was written for a capsule that has a relay")
		}
		body, err := os.ReadFile(filepath.Join(home, ".docker", "config.json"))
		if err != nil {
			t.Fatal(err)
		}
		var got struct {
			Proxies struct {
				Default struct {
					HTTPProxy  string `json:"httpProxy"`
					HTTPSProxy string `json:"httpsProxy"`
					NoProxy    string `json:"noProxy"`
				} `json:"default"`
			} `json:"proxies"`
		}
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("unmarshal %s: %v", body, err)
		}
		if got.Proxies.Default.HTTPProxy != env["HTTP_PROXY"] ||
			got.Proxies.Default.HTTPSProxy != env["HTTPS_PROXY"] ||
			got.Proxies.Default.NoProxy != env["NO_PROXY"] {
			t.Errorf("config = %s; want the capsule's own relay", body)
		}
	})

	t.Run("https falls back to the one proxy there is", func(t *testing.T) {
		home := t.TempDir()
		env := map[string]string{"HTTP_PROXY": "http://10.0.0.2:3128"}
		if _, err := writeDockerProxyConfig(func(k string) string { return env[k] }, home, uid, gid); err != nil {
			t.Fatal(err)
		}
		body, _ := os.ReadFile(filepath.Join(home, ".docker", "config.json"))
		if !strings.Contains(string(body), `"httpsProxy":"http://10.0.0.2:3128"`) {
			t.Errorf("config = %s; want https to fall back rather than be empty", body)
		}
	})

	t.Run("open egress writes nothing", func(t *testing.T) {
		home := t.TempDir()
		wrote, err := writeDockerProxyConfig(func(string) string { return "" }, home, uid, gid)
		if err != nil {
			t.Fatal(err)
		}
		if wrote {
			t.Error("a config was written for a capsule with no relay")
		}
		// Pointing a client at a proxy that does not exist would break
		// what works today.
		if _, err := os.Stat(filepath.Join(home, ".docker", "config.json")); !os.IsNotExist(err) {
			t.Errorf("stat: %v; want no file at all", err)
		}
	})
}

// TestTheReadinessProbeIsBounded: one probe cannot spend the whole
// readiness budget.
//
// A daemon that accepts the socket and then never answers leaves the
// probe blocked, and this process is PID 1 — there is nothing outside it
// to cut the call short. Without a bound of its own the budget is spent
// inside a single call, and the capsule reports neither ready nor
// failed until something kills the container.
func TestTheReadinessProbeIsBounded(t *testing.T) {
	// A `docker` on PATH that answers nothing, ever.
	dir := t.TempDir()
	shim := filepath.Join(dir, "docker")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\nsleep 60\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	start := time.Now()
	err := probeDockerd(t.Context())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a probe that never answered reported the daemon ready")
	}
	if elapsed > dockerdProbeTimeout+5*time.Second {
		t.Errorf("one probe took %s against a %s bound; the readiness budget is spent inside a single call",
			elapsed.Round(time.Second), dockerdProbeTimeout)
	}
}

// TestAControlFileIsNeverReadHalfWritten: a reader racing a writer of the
// control surface sees the previous contents or the next, never nothing.
//
// The launcher reads these files from outside the container while the
// supervisor writes them from inside, with no lock and no handshake
// between them — it `cat`s the protocol declaration during boot and
// execs `supervisor state` on every poll of a running capsule. A write
// that truncates first gives that reader an empty file that reads as a
// successful answer, and an empty answer is not "not yet": an empty
// protocol declaration is a version this controller does not speak, and
// an empty state is a state nothing recognizes. Both park a healthy
// capsule.
//
// It is fixed in the writer rather than by retrying in the reader. The
// reader cannot tell a file caught mid-write from one with the wrong
// contents, and a retry would leave the same window for every other
// reader of the control surface.
func TestAControlFileIsNeverReadHalfWritten(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "protocol")
	// Two payloads of very different lengths: a torn read between two
	// equal ones can look intact, and it is the tail of the longer that
	// makes a partial write visible.
	payloads := [][]byte{[]byte("2"), []byte(strings.Repeat("9", 4096))}
	if err := writeControlFile(path, payloads[0], 0o644, -1, -1); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	bad := make(chan string, 1)
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				// A rename never leaves the target absent, so a missing
				// file is a finding rather than something to poll past.
				select {
				case bad <- "unreadable: " + err.Error():
				default:
				}
				return
			}
			whole := false
			for _, want := range payloads {
				if string(raw) == string(want) {
					whole = true
				}
			}
			if !whole {
				select {
				case bad <- fmt.Sprintf("%d bytes: %.32q", len(raw), raw):
				default:
				}
				return
			}
		}
	}()

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 40 {
				if err := writeControlFile(path, payloads[i%len(payloads)], 0o644, -1, -1); err != nil {
					t.Errorf("write: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(stop)

	select {
	case raw := <-bad:
		t.Fatalf("a reader observed a control file that is neither payload — %s", raw)
	default:
	}
}
