package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/rhobuild/runpool/internal/capsule/protocol"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
// once fork/exec has returned, so it is the only proof the supervisor has
// that the job was handed over. A failure before it must not
// be reported as an execution, or the controller settles a job that never ran.
func TestTerminalFailureDistinguishesUnstartedRunners(t *testing.T) {
	err := errors.New("no credential was delivered")

	for _, state := range []string{"", "waiting", "starting"} {
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
	err := probeDockerd(t.Context(), testReaper())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a probe that never answered reported the daemon ready")
	}
	if elapsed > dockerdProbeTimeout+5*time.Second {
		t.Errorf("one probe took %s against a %s bound; the readiness budget is spent inside a single call",
			elapsed.Round(time.Second), dockerdProbeTimeout)
	}
}

// TestAStateThatCannotBeReplacedIsStillWritten: when the atomic
// replacement fails, the state is written in place rather than not at
// all.
//
// The two failures are not equal, and this is which one to take. A
// reader catching the file mid-write reports the capsule as
// unobservable, and the attempt is held for a person to look at. A state
// left stale reports a running job as one that never started —
// terminalFailure reads anything but `running` as aborted — and the
// controller requeues it, so the customer's job runs twice.
//
// The failure is reachable: the atomic replacement needs a new inode
// where an in-place write reuses the block already there, and the
// control directory is a one-megabyte tmpfs shared with a credential
// that may be a megabyte itself.
func TestAStateThatCannotBeReplacedIsStillWritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state")
	if err := os.WriteFile(path, []byte("waiting"), 0o644); err != nil {
		t.Fatal(err)
	}

	replaceState(path, "running", func(string, []byte, os.FileMode, int, int) error {
		return errors.New("no space left on device")
	})

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "running" {
		t.Errorf("state = %q after a replacement that could not be made; want running. "+
			"A stale state is read as a job that never ran, and the controller runs it again", raw)
	}
}

// TestEveryFileIsWrittenThroughTheAtomicHelper: the supervisor creates
// no file except through atomicfile, save one deliberate exception.
//
// A helper only helps the writes that use it, and a test of the helper
// passes with none of them converted — so this checks the call sites.
//
// It parses rather than greps, because grepping was tried and did both
// things wrong. It named the four control-file constants, so the two
// writes whose target is a local variable were invisible to it. Counting
// the string instead saw those, and then could not tell a call from a
// comment naming one, so the file could not describe its own rule.
//
// The exception is replaceState's in-place fallback, which exists
// because a state that cannot be written is worse than one written
// half-way; it is named here so adding a second exception is a decision
// somebody makes rather than a count that still passes.
func TestEveryFileIsWrittenThroughTheAtomicHelper(t *testing.T) {
	const allowedIn = "replaceState"
	creators := map[string]bool{"WriteFile": true, "Create": true, "OpenFile": true}

	sources, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()

	var found int
	for _, src := range sources {
		if strings.HasSuffix(src, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, src, nil, 0)
		if err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkgName, ok := sel.X.(*ast.Ident)
				if !ok || pkgName.Name != "os" || !creators[sel.Sel.Name] {
					return true
				}
				found++
				if fn.Name.Name != allowedIn {
					t.Errorf("%s: os.%s in %s. Every file this binary writes is read by "+
						"something that did not wait for it, so it goes through "+
						"atomicfile.Replace; %s is the one exception and says why",
						fset.Position(call.Pos()), sel.Sel.Name, fn.Name.Name, allowedIn)
				}
				return true
			})
		}
	}
	if len(sources) == 0 {
		t.Fatal("no sources found; this test asserted nothing")
	}
	if found == 0 {
		t.Error("no file-creating call found at all; this test asserted nothing")
	}
}

// TestTheStartAuthorizationRecordsItselfBeforeItLands: the state is
// written before the file that carries the authorization.
//
// PID 1 learns of an authorization only by polling for that file, and it
// writes nothing of its own until fork/exec has returned. Between those
// two moments the capsule answers with whatever state was last written,
// and if that is still `waiting` a launcher reads it as proof no runner
// ever started: an authorization whose exec landed but whose call
// returned an error requeues an assignment this capsule is at that
// moment starting a runner for.
func TestTheStartAuthorizationRecordsItselfBeforeItLands(t *testing.T) {
	var wrote []string
	record := func(path string, body []byte, _ os.FileMode, _, _ int) error {
		wrote = append(wrote, filepath.Base(path)+"="+string(body))
		return nil
	}
	if err := authorizeStart(record); err != nil {
		t.Fatal(err)
	}
	want := []string{"state=" + protocol.StateStarting, "start=" + protocolVersion}
	if !slices.Equal(wrote, want) {
		t.Errorf("the authorization wrote %v; want %v.\nUntil the state is recorded the capsule "+
			"answers `waiting`, which is read as proof that no runner ever started.", wrote, want)
	}
}

// TestAnAuthorizationThatCannotLandSaysSo: a file that never appears
// leaves the state where an assignment can still be served again.
//
// This is the one place that knows nothing took effect, and it is the
// likely shape of a full control tmpfs rather than a remote one: on a
// full one the state's fallback truncates the file already there and
// writes into the page that frees, where the authorization has no old
// value to reclaim. Left saying `starting`, an assignment that could
// simply be requeued is held for a person instead.
func TestAnAuthorizationThatCannotLandSaysSo(t *testing.T) {
	full := errors.New("no space left on device")
	var wrote []string
	record := func(path string, body []byte, _ os.FileMode, _, _ int) error {
		if filepath.Base(path) == "start" {
			return full
		}
		wrote = append(wrote, string(body))
		return nil
	}
	if err := authorizeStart(record); !errors.Is(err, full) {
		t.Fatalf("authorize returned %v; want the write's own failure", err)
	}
	if len(wrote) == 0 || wrote[len(wrote)-1] != protocol.StateWaiting {
		t.Errorf("the capsule was left saying %v after an authorization that never landed; "+
			"it has to say the state a launcher requeues from", wrote)
	}
}

// TestTheStartSubcommandAuthorizesThroughTheOrderedPath: the two
// properties above belong to authorizeStart, so the verb has to go
// through it.
//
// Writing the start file from the clause directly is a working
// authorization with none of the ordering: the capsule answers `waiting`
// for the whole preamble again, and nothing else would notice, because
// the function keeps its own tests either way.
func TestTheStartSubcommandAuthorizesThroughTheOrderedPath(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var clause *ast.CaseClause
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "runSubcommand" {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			c, ok := n.(*ast.CaseClause)
			if !ok || clause != nil {
				return true
			}
			for _, expr := range c.List {
				if lit, ok := expr.(*ast.BasicLit); ok && lit.Value == `"start"` {
					clause = c
				}
			}
			return true
		})
	}
	if clause == nil {
		t.Fatal("no `start` subcommand in the dispatcher; there is nothing here to authorize with")
	}
	authorizes, writesTheFileItself := false, false
	ast.Inspect(clause, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok {
			return true
		}
		switch id.Name {
		case "authorizeStart":
			authorizes = true
		case "startFile", "stateFile":
			// The authorization owns both names. A clause that still
			// reaches for one is writing the control surface beside the
			// function that orders those writes -- which is how a call
			// left in a branch nothing takes reads as authorizing while
			// the line below it does the real work, unordered.
			writesTheFileItself = true
		}
		return true
	})
	if !authorizes {
		t.Errorf("%s: the start subcommand does not authorize through authorizeStart, so the "+
			"order of the state and the file, and the undo when the file cannot land, are "+
			"whatever this clause happens to do", fset.Position(clause.Pos()))
	}
	if writesTheFileItself {
		t.Errorf("%s: the start subcommand names the control files itself. Whatever it does with "+
			"them is outside the order authorizeStart exists to keep, and a call to it can sit "+
			"beside that and prove nothing", fset.Position(clause.Pos()))
	}
}

// TestDeliverIsTheCredentialChannel: the bundle lands 0600 with exactly
// the bytes that arrived, an empty delivery is refused, and one past the
// bound is refused rather than silently truncated — a truncated
// credential is a corrupt one the runner fails on later with nothing
// naming the cause. Nothing here covered this verb at all, and it is the
// channel the JIT credential crosses.
func TestDeliverIsTheCredentialChannel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jitconfig")
	var stderr bytes.Buffer

	if code := deliver(strings.NewReader(""), &stderr, path, -1, -1); code != 1 {
		t.Errorf("an empty delivery returned %d; want refusal", code)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("a refused delivery left a file behind")
	}

	if code := deliver(strings.NewReader(strings.Repeat("x", maxJITBundle+1)), &stderr, path, -1, -1); code != 1 {
		t.Errorf("a bundle past the bound returned %d; want refusal, not truncation", code)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("an oversized delivery left a truncated credential behind")
	}

	bundle := `{"runner":{},"credentials":{}}`
	if code := deliver(strings.NewReader(bundle), &stderr, path, -1, -1); code != 0 {
		t.Fatalf("deliver = %d, stderr %q", code, stderr.String())
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != bundle {
		t.Errorf("the file holds %q, %v; want the bytes that arrived", got, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v; want 0600 — the bundle is readable by whoever the mode admits", info.Mode().Perm())
	}
}

// TestTheReaperDeliversEachTrackedExitOnce: the reaper is the one place
// that calls wait4, and a wrong status here is a wrong disposition — the
// runner's exit code is the whole of what the observation machine rules
// on. A child that dies instantly must still be reaped as tracked,
// because registration and the reap loop hold the same lock.
//
// It runs real children, so nothing else in this package may exec
// concurrently: the reaper's wait4(-1) would steal their statuses. The
// package's other tests are pure, which is what makes this one safe.
func TestTheReaperDeliversEachTrackedExitOnce(t *testing.T) {
	r := testReaper()

	ch, err := r.start(exec.Command("true"))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-ch:
		if code != 0 {
			t.Errorf("true exited %d; want 0", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the tracked exit never arrived; a child that died instantly was reaped as an orphan")
	}

	cmd := exec.Command("sh", "-c", "exit 7")
	ch, err = r.start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-ch:
		if code != 7 {
			t.Errorf("the child exited %d; want its own 7 — a wrong status is a wrong disposition", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the tracked exit never arrived")
	}
}

// testReaper is the one reaper the package's tests share. Two reapers in
// one process are two wait4(-1) loops racing for every child — the exact
// hazard the reaper exists to remove — and under shuffle the loser's
// tracked channel never fires. The production process has one; the test
// process gets one.
var testReaper = sync.OnceValue(newReaper)
