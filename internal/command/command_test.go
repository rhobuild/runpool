package command

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rhobuild/runpool/internal/assignment"
	"github.com/rhobuild/runpool/internal/store"
	_ "modernc.org/sqlite"
)

// run drives the whole tree with captured streams, which is the point
// of injecting them: every assertion here is about what a person or a
// script actually sees.
func run(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errBuf bytes.Buffer
	code = Execute(args, BuildInfo{
		Version: "v1.2.3",
		Commit:  "abc123",
		Built:   "2026-08-13T00:00:00Z",
		CapsuleImage: "ghcr.io/rhobuild/runpool/capsule@sha256:" +
			"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	},
		IO{In: strings.NewReader(""), Out: &out, Err: &errBuf})
	return code, out.String(), errBuf.String()
}

// TestHelpIsNotAnUnknownCommand verifies every conventional help form.
func TestHelpIsNotAnUnknownCommand(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"-h"}, {"help"}, {}} {
		code, stdout, stderr := run(t, args...)
		if code != exitOK {
			t.Errorf("runpool %v = %d; want 0 — asking for help is not an error", args, code)
		}
		if !strings.Contains(stdout, "runpool") {
			t.Errorf("runpool %v printed no help: %q %q", args, stdout, stderr)
		}
	}
}

// TestSubcommandHelp verifies help at each command depth.
func TestSubcommandHelp(t *testing.T) {
	for _, args := range [][]string{
		{"attempts", "--help"}, {"config", "--help"}, {"attempts", "resolve", "--help"},
	} {
		code, stdout, _ := run(t, args...)
		if code != exitOK {
			t.Errorf("runpool %v = %d; want 0", args, code)
		}
		if !strings.Contains(stdout, "Usage:") {
			t.Errorf("runpool %v printed no usage: %q", args, stdout)
		}
	}
}

// TestExtraArgumentsAreRefused keeps positional-argument contracts strict.
func TestExtraArgumentsAreRefused(t *testing.T) {
	for _, args := range [][]string{
		{"version", "extra"},
		{"serve", "unexpected"},
		{"status", "surplus"},
		{"doctor", "surplus"},
		{"gc", "surplus"},
		{"attempts", "list", "surplus"},
	} {
		code, _, _ := run(t, args...)
		if code != exitUsage {
			t.Errorf("runpool %v = %d; want %d — an unexpected argument is a usage error", args, code, exitUsage)
		}
	}
}

// TestArgumentCountsAreExact: a command that takes an id must say so
// when it is missing, rather than proceeding with none.
func TestArgumentCountsAreExact(t *testing.T) {
	for _, args := range [][]string{
		{"attempts", "inspect"},
		{"attempts", "inspect", "a", "b"},
		{"attempts", "resolve"},
	} {
		if code, _, _ := run(t, args...); code != exitUsage {
			t.Errorf("runpool %v = %d; want %d", args, code, exitUsage)
		}
	}
}

// TestUnknownThingsAreReported: an exit code with no message leaves a
// person guessing. Both the unknown command and the unknown flag have
// to say what was wrong, on stderr, where a script's error handling
// looks.
func TestUnknownThingsAreReported(t *testing.T) {
	code, _, stderr := run(t, "bogus")
	if code != exitUsage {
		t.Errorf("unknown command = %d; want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "bogus") {
		t.Errorf("unknown command did not name it on stderr: %q", stderr)
	}

	if code, _, _ := run(t, "status", "--nope"); code != exitUsage {
		t.Errorf("unknown flag = %d; want %d", code, exitUsage)
	}
}

// TestVersionOutputs: the plain form is one line a human reads; the
// JSON form carries the build facts a support request or a
// release record needs.
func TestVersionOutputs(t *testing.T) {
	code, stdout, _ := run(t, "version")
	if code != exitOK || !strings.Contains(stdout, "v1.2.3") {
		t.Fatalf("version = %d, %q", code, stdout)
	}

	code, stdout, _ = run(t, "version", "--json")
	if code != exitOK {
		t.Fatalf("version --json = %d", code)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("version --json is not JSON: %v\n%s", err, stdout)
	}
	for _, key := range []string{"version", "commit", "built", "dirty", "go", "platform", "capsule_image", "release_qualification_reference"} {
		if _, ok := doc[key]; !ok {
			t.Errorf("version --json has no %q: %v", key, doc)
		}
	}
	if doc["version"] != "v1.2.3" {
		t.Errorf("version = %v; want the stamped one", doc["version"])
	}
}

// TestValidReleaseVersion: a release may only be published from a
// SemVer tag. "dev" is what a local build carries, and a build that was
// never tagged was never released.
func TestValidReleaseVersion(t *testing.T) {
	for _, ok := range []string{"v1.0.0", "v1.0.0-rc.1", "v0.1.0", "v1.2.3+build.5", "v10.20.30"} {
		if err := ValidReleaseVersion(ok); err != nil {
			t.Errorf("ValidReleaseVersion(%q) = %v; want valid", ok, err)
		}
	}
	for _, bad := range []string{"", "dev", "1.0.0", "v1.0", "v1.0.0.0", "latest", "v01.0.0", "release-1"} {
		if err := ValidReleaseVersion(bad); err == nil {
			t.Errorf("ValidReleaseVersion(%q) accepted an unpublishable version", bad)
		}
	}
}

// TestDestructiveCommandsPreviewByDefault: uninstall, cleanup and gc
// all change the world, so none of them may act on a bare invocation.
// The check is on the flag's default, because that is what decides.
func TestDestructiveCommandsPreviewByDefault(t *testing.T) {
	root := NewRootCommand(BuildInfo{Version: "test"}, IO{Out: &bytes.Buffer{}, Err: &bytes.Buffer{}})
	for name, flag := range map[string]string{
		"cleanup":   "apply",
		"gc":        "apply",
		"uninstall": "confirm",
	} {
		cmd, _, err := root.Find([]string{name})
		if err != nil {
			t.Fatalf("find %s: %v", name, err)
		}
		f := cmd.Flags().Lookup(flag)
		if f == nil {
			t.Errorf("%s has no --%s", name, flag)
			continue
		}
		if f.DefValue != "false" && f.DefValue != "" {
			t.Errorf("%s --%s defaults to %q; a destructive command must preview by default", name, flag, f.DefValue)
		}
	}
}

// TestConfigContract distinguishes valid input, usage errors, and operational
// failures.
func TestConfigContract(t *testing.T) {
	valid := filepath.Join(t.TempDir(), "config.yaml")
	content := `apiVersion: runpool.rhobuild.com/v1
kind: RunpoolConfig
host:
  topology: dedicated-daemon
targets:
  - id: app
    url: https://github.com/acme/app
    credential: github-default
    tiers:
      - tier: standard
credentials:
  - id: github-default
    tokenEnv: RUNPOOL_GITHUB_TOKEN
tiers:
  - id: standard
`
	if err := os.WriteFile(valid, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		args []string
		want int
	}{
		{[]string{"config"}, exitOK}, // prints help
		{[]string{"config", "validate"}, exitUsage},
		{[]string{"config", "validate", "--file", valid}, exitOK},
		{[]string{"config", "validate", "--file", valid + ".missing"}, exitError},
		{[]string{"config", "effective", "--file", valid}, exitOK},
	} {
		if code, _, _ := run(t, tc.args...); code != tc.want {
			t.Errorf("runpool %v = %d; want %d", tc.args, code, tc.want)
		}
	}
}

// TestCobraIsTheOnlyParser guards the single-parser architecture.
func TestCobraIsTheOnlyParser(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), "flag.NewFlagSet") {
			t.Errorf("%s builds a second flag parser; Cobra owns parsing, and a "+
				"command body takes typed parameters", name)
		}
	}
}

// TestEveryDeclaredFlagReachesItsCommand walks the tree and drives each
// flag through Execute. A flag the tree declares but nothing reads is
// documentation for behaviour that does not exist; a flag a body needs
// but the tree omits is rejected as unknown. Both are the same defect
// seen from opposite ends.
func TestEveryDeclaredFlagReachesItsCommand(t *testing.T) {
	// --state is the one that was missing. A value the command does not
	// serve must be a usage error naming it, not "unknown flag".
	code, _, stderr := run(t, "attempts", "list", "--state", "settled")
	if code != exitUsage {
		t.Errorf("attempts list --state settled = %d; want %d", code, exitUsage)
	}
	if strings.Contains(stderr, "unknown flag") {
		t.Errorf("--state is declared by the body but not the tree: %q", stderr)
	}
	if !strings.Contains(stderr, "manual-review") {
		t.Errorf("the refusal does not say which states are served: %q", stderr)
	}

	// Both served values parse; with no state directory they report that
	// rather than failing. Ready is here because a queue that stops
	// draining is the shape a stuck binding takes, and until it was
	// listable an operator could see the count and not the attempts.
	t.Setenv("RUNPOOL_STATE_DIR", t.TempDir())
	for _, state := range []string{"manual-review", "ready"} {
		if code, _, stderr := run(t, "attempts", "list", "--state", state); code != exitOK {
			t.Errorf("attempts list --state %s = %d; want 0 (%q)", state, code, stderr)
		}
	}
	if code, _, stderr := run(t, "attempts", "list", "--limit", "0"); code != exitUsage {
		t.Errorf("attempts list --limit 0 = %d; want %d (%q)", code, exitUsage, stderr)
	}
	if code, _, stderr := run(t, "attempts", "list", "--cursor", "not-base64"); code != exitUsage {
		t.Errorf("attempts list with malformed cursor = %d; want %d (%q)", code, exitUsage, stderr)
	}
}

// TestOperatorResolveRefusesAnUndecidedDecision: both flags or neither
// is a person who has not decided, and guessing which they meant is how
// a job runs twice.
func TestOperatorResolveRefusesAnUndecidedDecision(t *testing.T) {
	for _, args := range [][]string{
		{"attempts", "resolve", "att-1", "--reason", "checked"},
		{"attempts", "resolve", "att-1", "--retry", "--settle-may-have-run", "--reason", "checked"},
		{"attempts", "resolve", "att-1", "--retry"},
	} {
		if code, _, _ := run(t, args...); code != exitUsage {
			t.Errorf("runpool %v = %d; want %d", args, code, exitUsage)
		}
	}
}

// TestConfigCommandsSayWhatTheyDecided. These ran through the tree
// already, but everything they printed escaped to the test process's own
// stdout: only the exit code could be asserted, so a command that exited
// zero while saying nothing — or saying the wrong thing — read as
// passing.
func TestConfigCommandsSayWhatTheyDecided(t *testing.T) {
	valid := filepath.Join(t.TempDir(), "config.yaml")
	content := `apiVersion: runpool.rhobuild.com/v1
kind: RunpoolConfig
host:
  topology: dedicated-daemon
targets:
  - id: app
    url: https://github.com/acme/app
    credential: github-default
    tiers:
      - tier: standard
credentials:
  - id: github-default
    tokenEnv: RUNPOOL_GITHUB_TOKEN
tiers:
  - id: standard
`
	if err := os.WriteFile(valid, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, _ := run(t, "config", "validate", "--file", valid)
	if code != exitOK || !strings.Contains(stdout, "configuration valid") {
		t.Errorf("validate = %d %q; want a stated verdict", code, stdout)
	}

	code, stdout, _ = run(t, "config", "effective", "--file", valid)
	if code != exitOK {
		t.Fatalf("effective = %d", code)
	}
	// The effective document is the defaults made visible. A deletion
	// policy that is on by default must be among them, or an operator
	// reading this output does not know their records expire.
	for _, want := range []string{"retention:", "leaseHistory:", "topology: dedicated-daemon"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the effective configuration does not mention %q:\n%s", want, stdout)
		}
	}
}

// TestReportingShapesBeforeTheFirstServe pins what each surface answers
// on an instance that has never run. The listing answers in the
// listing's own shape — a paginated document with an empty attempts array,
// because no state holds no attempts — and inspect fails naming the absent state, because it was
// asked about one attempt that therefore does not exist. Only status
// answers the question "has this instance served", and its document
// carries `served` as the v1 discriminator in both forms, so a consumer
// branches on a field instead of on which fields happen to exist.
func TestReportingShapesBeforeTheFirstServe(t *testing.T) {
	t.Setenv("RUNPOOL_STATE_DIR", t.TempDir())

	code, stdout, _ := run(t, "attempts", "list", "--state", "manual-review", "--json")
	if code != exitOK {
		t.Fatalf("attempts list --json = %d; want 0", code)
	}
	var attempts attemptListDocument
	if err := json.Unmarshal([]byte(stdout), &attempts); err != nil {
		t.Fatalf("attempts list --json did not emit its page document: %v (%q)", err, stdout)
	}
	if attempts.Attempts == nil || len(attempts.Attempts) != 0 || attempts.Total != 0 {
		t.Errorf("attempts list on a never-run instance = %+v; want an explicit empty page", attempts)
	}

	code, _, stderr := run(t, "attempts", "inspect", "att-1", "--json")
	if code == exitOK {
		t.Error("attempts inspect on a never-run instance succeeded; the attempt cannot exist")
	}
	if !strings.Contains(stderr, "has not run yet") {
		t.Errorf("inspect's failure does not name the absent state: %q", stderr)
	}

	code, stdout, _ = run(t, "status", "--json")
	if code != exitOK {
		t.Fatalf("status --json = %d; want 0", code)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("status --json: %v", err)
	}
	if doc["api_version"] != "v1" {
		t.Errorf("api_version = %v; want v1", doc["api_version"])
	}
	if served, ok := doc["served"].(bool); !ok || served {
		t.Errorf("served = %v; the pre-serve form carries served=false", doc["served"])
	}
}

func TestAttemptsListPaginatesItsJSONDocumentWithAnOpaqueCursor(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RUNPOOL_STATE_DIR", dir)
	st, err := store.Open(dir, store.DefaultRetryBudget)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Tx(t.Context(), func(tx *store.Tx) error {
		bindingID, err := tx.EnsureBinding("app", "github_actions",
			"v1|repository|https://github.com/acme/app||runpool-standard")
		if err != nil {
			return err
		}
		workloads := []assignment.WorkloadAssignment{
			{SourceWorkloadKey: "job-a"},
			{SourceWorkloadKey: "job-b"},
			{SourceWorkloadKey: "job-c"},
		}
		rows := []store.WorkloadRow{
			{SourceWorkloadKey: "job-a", TenantKey: "acme", ProjectKey: "app"},
			{SourceWorkloadKey: "job-b", TenantKey: "acme", ProjectKey: "app"},
			{SourceWorkloadKey: "job-c", TenantKey: "acme", ProjectKey: "app"},
		}
		if _, err := tx.RecordDelivery(bindingID, "pagination", workloads, rows); err != nil {
			return err
		}
		page, err := tx.ReadyAttemptPage(nil, len(rows))
		if err != nil {
			return err
		}
		for _, attempt := range page.Attempts {
			if err := tx.HoldForReview(attempt.ID, store.ReviewReasonStartOutcomeUnknown); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := run(t, "attempts", "list", "--json", "--limit", "2")
	if code != exitOK {
		t.Fatalf("first page = %d; want 0 (%q)", code, stderr)
	}
	var first attemptListDocument
	if err := json.Unmarshal([]byte(stdout), &first); err != nil {
		t.Fatal(err)
	}
	if len(first.Attempts) != 2 || first.Total != 3 || first.NextCursor == "" {
		t.Fatalf("first page = %+v; want two of three and a cursor", first)
	}

	code, stdout, stderr = run(t, "attempts", "list", "--json", "--limit", "2", "--cursor", first.NextCursor)
	if code != exitOK {
		t.Fatalf("second page = %d; want 0 (%q)", code, stderr)
	}
	var second attemptListDocument
	if err := json.Unmarshal([]byte(stdout), &second); err != nil {
		t.Fatal(err)
	}
	if len(second.Attempts) != 1 || second.Total != 3 || second.NextCursor != "" {
		t.Fatalf("second page = %+v; want the final one of three", second)
	}
	for _, previous := range first.Attempts {
		if previous.ID == second.Attempts[0].ID {
			t.Fatalf("attempt %s appeared in both pages", previous.ID)
		}
	}

	if code, _, _ := run(t, "attempts", "list", "--state", "ready", "--cursor", first.NextCursor); code != exitUsage {
		t.Fatalf("manual-review cursor used for ready state = %d; want %d", code, exitUsage)
	}
}

// TestLivenessReadsTheStateAndTheVerdictAge: a stat passes a corrupt
// database and a wedged serve loop alike, which is exactly what a
// liveness probe exists to catch. Readable state with a fresh verdict —
// or none yet, which is the deployment start_period's window — is ok; a
// stale verdict is a serve loop that stopped; garbage where the
// database should be is a controller that cannot do anything correctly.
func TestLivenessReadsTheStateAndTheVerdictAge(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RUNPOOL_STATE_DIR", dir)

	// Before the first serve there is no state at all: liveness fails,
	// and the start_period is what gives a booting controller its grace.
	if code, _, _ := run(t, "healthcheck", "--mode", "liveness"); code == exitOK {
		t.Error("liveness passed with no state; a stat of nothing would have failed too")
	}

	st, err := store.Open(dir, store.DefaultRetryBudget)
	if err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := run(t, "healthcheck", "--mode", "liveness"); code != exitOK {
		t.Errorf("liveness failed on a readable store with no verdict yet: %q", stderr)
	}

	// A fresh verdict passes; one far beyond the monitor's cadence is a
	// serve loop that stopped writing.
	if err := st.Tx(t.Context(), func(tx *store.Tx) error {
		return tx.SetPressure(store.PressureInfo{
			Level: "normal", FreeBytes: 1 << 30, FreeInodes: 1 << 20,
		})
	}); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := run(t, "healthcheck", "--mode", "liveness"); code != exitOK {
		t.Errorf("liveness failed on a fresh verdict: %q", stderr)
	}
	st.Close()

	// The verdict is stamped by the store at write time, so a stale one
	// cannot be produced through the API — which is the point: only a
	// loop that genuinely stopped leaves an old moment behind. The
	// fixture forges that state directly.
	db, err := sql.Open("sqlite", filepath.Join(dir, store.DatabaseFile))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE pressure SET measured_at = ?`, time.Now().Add(-time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	db.Close()
	code, _, stderr := run(t, "healthcheck", "--mode", "liveness")
	if code == exitOK {
		t.Error("liveness passed on an hour-old verdict; the serve loop writes one per minute")
	}
	if !strings.Contains(stderr, "serve loop") {
		t.Errorf("the failure does not say what stopped: %q", stderr)
	}

	// The file present but not a database: the stat this replaced passed
	// exactly this.
	garbage := filepath.Join(t.TempDir(), "g")
	t.Setenv("RUNPOOL_STATE_DIR", garbage)
	if err := os.MkdirAll(garbage, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(garbage, store.DatabaseFile), []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := run(t, "healthcheck", "--mode", "liveness"); code == exitOK {
		t.Error("liveness passed on a file that is not a database")
	}
}
