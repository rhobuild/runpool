// Package consistency ties together values that live in different
// ecosystems and are read by different updaters: a Go constant and a
// Compose key, a reviewed lock and a sentence of documentation, a
// workflow label and a configuration default. None of them can see the
// others, so each pair drifts silently in a green build — and every
// check here replaces either a shell parser that broke on gofmt-legal
// edits or a comment that asked a reader to remember.
package consistency

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/rhobuild/runpool/internal/app"
	"github.com/rhobuild/runpool/internal/config"
)

func repoPath(parts ...string) string {
	return filepath.Join(append([]string{"..", ".."}, parts...)...)
}

// TestStopGracePeriodHoldsTheWholeShutdown: shutdown spends the drain
// window in full whenever work is in flight and then closes every
// message session under one shared budget, so the deployment's stop
// grace period must exceed the sum. When it does not, the platform's
// SIGKILL lands first, the deferred closes never run, and every restart
// with a live job leaves the broker holding a session the next start
// waits out as a conflict.
func TestStopGracePeriodHoldsTheWholeShutdown(t *testing.T) {
	raw, err := os.ReadFile(repoPath("deploy", "compose", "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var compose struct {
		Services map[string]struct {
			StopGracePeriod string `yaml:"stop_grace_period"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(raw, &compose); err != nil {
		t.Fatal(err)
	}
	controller, ok := compose.Services["controller"]
	if !ok || controller.StopGracePeriod == "" {
		t.Fatal("deploy/compose/compose.yaml declares no stop_grace_period for the controller")
	}
	grace, err := time.ParseDuration(controller.StopGracePeriod)
	if err != nil {
		// Unreadable is a failure, not a zero: a form this cannot parse
		// must not pass with a grace period of no seconds.
		t.Fatalf("stop_grace_period %q is not a duration: %v", controller.StopGracePeriod, err)
	}
	shutdown := app.DrainTimeout + app.SessionCloseBudget
	if grace <= shutdown {
		t.Fatalf("stop_grace_period %s does not hold the %s shutdown (%s drain + %s session close); "+
			"the platform kills the controller before it closes its sessions",
			grace, shutdown, app.DrainTimeout, app.SessionCloseBudget)
	}
}

// TestDocumentedEngineVersionsMatchTheLock: the reviewed lock names the
// engine a release is qualified against, and sentences in the support
// matrix, the changelog and the guides repeat it. A repeated version
// outlives a lock update unless something compares them — this is what
// once left one page selecting an engine no lock named. Decision
// records are excluded: they are point-in-time records, and rewriting
// their history to match a newer lock would falsify them.
func TestDocumentedEngineVersionsMatchTheLock(t *testing.T) {
	raw, err := os.ReadFile(repoPath("build", "platform.lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	var lock struct {
		Policy struct {
			TargetEngine string `json:"target_engine"`
		} `json:"policy"`
	}
	if err := json.Unmarshal(raw, &lock); err != nil {
		t.Fatal(err)
	}
	target := lock.Policy.TargetEngine
	if target == "" {
		t.Fatal("build/platform.lock.json names no target engine, so this proves nothing")
	}
	major := target[:strings.Index(target, ".")]

	out, err := exec.Command("git", "-C", repoPath(), "ls-files", "*.md").Output()
	if err != nil {
		t.Fatal(err)
	}
	version := regexp.MustCompile(`\b` + regexp.QuoteMeta(major) + `\.\d+\.\d+\b`)
	var stale []string
	for _, file := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.HasPrefix(file, "docs/adrs/") {
			continue
		}
		body, err := os.ReadFile(repoPath(file))
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range version.FindAllString(string(body), -1) {
			if m != target {
				stale = append(stale, fmt.Sprintf("%s mentions engine %s; the lock names %s", file, m, target))
			}
		}
	}
	for _, s := range stale {
		t.Error(s)
	}
}

// TestTheExampleWorkflowReachesTheExampleDeployment: the example
// workflow is the one place a reader copies `runs-on` from, and the
// example configuration is the deployment it must reach. The label a
// tier serves is derived — prefix plus tier id, or an explicit scale
// set name — so the pair can drift while both halves stay individually
// valid. actionlint's declaration is held to the same label, because a
// linter told to expect a label nobody serves approves workflows that
// queue forever.
func TestTheExampleWorkflowReachesTheExampleDeployment(t *testing.T) {
	raw, err := os.ReadFile(repoPath("deploy", "workflows", "example.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var wf struct {
		Jobs map[string]struct {
			RunsOn string `yaml:"runs-on"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatal(err)
	}
	if len(wf.Jobs) == 0 {
		t.Fatal("the example workflow declares no jobs, so this proves nothing")
	}

	cfg, err := config.LoadFile(repoPath("deploy", "compose", "config.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	served := map[string]bool{}
	for _, target := range cfg.Targets {
		for _, tb := range target.Tiers {
			served[tb.ScaleSetName] = true
		}
	}
	if !served[config.DefaultScaleSetPrefix+config.DefaultTierID] {
		t.Errorf("the example deployment does not serve the default label %q; "+
			"every quick start names it", config.DefaultScaleSetPrefix+config.DefaultTierID)
	}

	lintRaw, err := os.ReadFile(repoPath(".github", "actionlint.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var lint struct {
		SelfHostedRunner struct {
			Labels []string `yaml:"labels"`
		} `yaml:"self-hosted-runner"`
	}
	if err := yaml.Unmarshal(lintRaw, &lint); err != nil {
		t.Fatal(err)
	}
	declared := map[string]bool{}
	for _, l := range lint.SelfHostedRunner.Labels {
		declared[l] = true
	}

	for name, job := range wf.Jobs {
		if job.RunsOn == "" {
			t.Errorf("example job %q declares no runs-on", name)
			continue
		}
		if !served[job.RunsOn] {
			t.Errorf("example job %q runs on %q, which the example deployment does not serve (%v)",
				name, job.RunsOn, keys(served))
		}
		if !declared[job.RunsOn] {
			t.Errorf("example job %q runs on %q, which .github/actionlint.yaml does not declare",
				name, job.RunsOn)
		}
	}
}

func keys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
