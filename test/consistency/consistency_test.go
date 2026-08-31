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
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/rhobuild/runpool/internal/app"
	"github.com/rhobuild/runpool/internal/config"
	"github.com/rhobuild/runpool/internal/store"
)

func repoPath(parts ...string) string {
	return filepath.Join(append([]string{"..", ".."}, parts...)...)
}

// TestStopGracePeriodHoldsTheWholeShutdown: shutdown waits out the serve
// loops, spends the drain window in full whenever work is in flight, and
// then closes every message session under one shared budget — so the
// deployment's stop grace period must exceed all three. Comparing
// against a subset of the terms is how a bound comes to describe less
// than the process actually does. When it does not, the platform's
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
	if grace <= app.ShutdownBudget {
		t.Fatalf("stop_grace_period %s does not hold the %s shutdown "+
			"(%s waiting for the loops + %s drain + %s session close); "+
			"the platform kills the controller before it closes its sessions",
			grace, app.ShutdownBudget, app.LoopStopBudget, app.DrainTimeout, app.SessionCloseBudget)
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
		Platforms []struct {
			Policy struct {
				Arch         string `json:"arch"`
				TargetEngine string `json:"target_engine"`
			} `json:"policy"`
		} `json:"platforms"`
	}
	if err := json.Unmarshal(raw, &lock); err != nil {
		t.Fatal(err)
	}
	if len(lock.Platforms) == 0 {
		t.Fatal("build/platform.lock.json names no platform at all, so this proves nothing")
	}
	// Every platform's engine, not the first one's. They are selected by
	// one policy today and need not stay that way, and a page naming a
	// version no platform selects is the thing this exists to catch.
	targets := map[string]bool{}
	for _, p := range lock.Platforms {
		if p.Policy.TargetEngine == "" {
			t.Fatalf("the %s entry names no target engine, so this proves nothing about it",
				p.Policy.Arch)
		}
		targets[p.Policy.TargetEngine] = true
	}
	// Every selected engine, not one. Two platforms qualified months
	// apart legitimately pin different ones, and that is what a
	// per-entry target is for -- so a document naming any of them is
	// current, and only a version naming none is stale.
	majors := map[string]bool{}
	for target := range targets {
		majors[target[:strings.Index(target, ".")]] = true
	}

	out, err := exec.Command("git", "-C", repoPath(), "ls-files", "*.md").Output()
	if err != nil {
		t.Fatal(err)
	}
	alternatives := make([]string, 0, len(majors))
	for major := range majors {
		alternatives = append(alternatives, regexp.QuoteMeta(major))
	}
	slices.Sort(alternatives)
	version := regexp.MustCompile(`\b(?:` + strings.Join(alternatives, "|") + `)\.\d+\.\d+\b`)
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
			if !targets[m] {
				selected := make([]string, 0, len(targets))
				for engine := range targets {
					selected = append(selected, engine)
				}
				slices.Sort(selected)
				stale = append(stale, fmt.Sprintf("%s mentions engine %s; the lock selects %s",
					file, m, strings.Join(selected, ", ")))
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

// TestTheTwoRetryBudgetDefaultsAgree: the deployment's default lives in
// config, the maintenance commands' default lives in store, and an
// import in either direction would put deployment vocabulary in the
// store or persistence in the schema. Two constants, one value, held
// here.
func TestTheTwoRetryBudgetDefaultsAgree(t *testing.T) {
	if config.DefaultRetryBudget != store.DefaultRetryBudget {
		t.Fatalf("config.DefaultRetryBudget = %d, store.DefaultRetryBudget = %d; "+
			"a deployment and a maintenance command would enforce different budgets",
			config.DefaultRetryBudget, store.DefaultRetryBudget)
	}
}

// TestNoDocCommentBelongsToAnotherDeclaration: a doc comment sits on the
// thing it names.
//
// Inserting a declaration between a comment and the declaration it
// documents silently reassigns the comment. The result compiles, gofmt
// says nothing, and `go vet` says nothing — staticcheck's ST1020 checks
// only exported names, and this codebase is mostly unexported. What a
// reader gets is `go doc` reporting one function's purpose under another
// function's name, and the next person to change either reasons from a
// sentence that was never about it.
//
// The rule is narrow on purpose. A doc comment that simply does not open
// with its own name is a style question and there are legitimate ones
// here. A doc comment that opens with the name of a *different*
// declaration in the same file is not a style question: it is a comment
// that was detached from what it describes.
func TestNoDocCommentBelongsToAnotherDeclaration(t *testing.T) {
	fset := token.NewFileSet()
	var found []string

	err := filepath.WalkDir(repoPath(), func(path string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir():
			// Generated code carries whatever its generator emits, and
			// vendored trees are not ours to describe.
			if name := d.Name(); name == ".git" || name == "sqlitedb" || name == "vendor" {
				return filepath.SkipDir
			}
			return nil
		case !strings.HasSuffix(path, ".go"):
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}

		// Every name this file declares, so the check can tell "a
		// different declaration" from an ordinary first word.
		declared := map[string]bool{}
		ast.Inspect(file, func(n ast.Node) bool {
			switch decl := n.(type) {
			case *ast.FuncDecl:
				declared[decl.Name.Name] = true
			case *ast.TypeSpec:
				declared[decl.Name.Name] = true
			case *ast.ValueSpec:
				for _, id := range decl.Names {
					declared[id.Name] = true
				}
			}
			return true
		})

		check := func(doc *ast.CommentGroup, name string, pos token.Pos) {
			if doc == nil || len(doc.List) == 0 {
				return
			}
			words := strings.Fields(strings.TrimPrefix(doc.List[0].Text, "//"))
			if len(words) == 0 {
				return
			}
			first := strings.Trim(words[0], "`*.,:")
			if first != name && declared[first] {
				found = append(found, fmt.Sprintf(
					"%s: the doc comment on %s begins with %q, which is another declaration in this file. "+
						"Something was inserted between %q's comment and %q",
					fset.Position(pos), name, first, first, first))
				return
			}
			// The same detachment happens by deletion, and the rule above
			// cannot see it: a comment left behind by a declaration that
			// no longer exists opens with a name nothing declares any
			// more. What gives it away is where this declaration's own
			// introduction sits. A comment that opens by naming its
			// subject is describing it from the first line, and a later
			// paragraph starting with the same word is prose — "Version 2
			// moved ..." under Version. A comment that opens with some
			// other word and then introduces this one partway down has
			// two subjects, and only the second is here.
			if first == name {
				return
			}
			for i, line := range doc.List {
				if i == 0 {
					continue
				}
				text := strings.Fields(strings.TrimPrefix(line.Text, "//"))
				if len(text) == 0 || strings.Trim(text[0], "`*.,:") != name {
					continue
				}
				// A sentence opening, not a wrapped line that happens to
				// break before the name. Requiring a blank line above
				// would miss the shape this is for: a deleted
				// declaration leaves its comment butted straight against
				// the next one, with no blank line anywhere.
				prev := strings.TrimSpace(strings.TrimPrefix(doc.List[i-1].Text, "//"))
				if prev != "" && !strings.HasSuffix(prev, ".") && !strings.HasSuffix(prev, ":") {
					continue
				}
				found = append(found, fmt.Sprintf(
					"%s: the doc comment on %s introduces it partway through, at %s. "+
						"Everything above that line describes something else, and was left "+
						"behind when whatever it named went away",
					fset.Position(pos), name, fset.Position(line.Pos())))
				return
			}
		}
		for _, decl := range file.Decls {
			switch decl := decl.(type) {
			case *ast.FuncDecl:
				check(decl.Doc, decl.Name.Name, decl.Pos())
			case *ast.GenDecl:
				// Only single-spec blocks: a grouped const or var block
				// documents the group, not its first member.
				if len(decl.Specs) != 1 {
					continue
				}
				switch spec := decl.Specs[0].(type) {
				case *ast.TypeSpec:
					check(decl.Doc, spec.Name.Name, decl.Pos())
				case *ast.ValueSpec:
					if len(spec.Names) > 0 {
						check(decl.Doc, spec.Names[0].Name, decl.Pos())
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range found {
		t.Error(f)
	}
}

// TestTheDeploymentSaysWhichHealthModeItWires: the deployment guide's
// platform checklist tells an operator what the reference Compose file
// asks the controller for, and the two are separate files.
//
// The checklist once asked for both liveness and readiness while the
// manifest wired liveness alone, so a platform maintainer building
// against the checklist would have wired a mode the reference does not
// use. Which of the two is right is a decision; that they say the same
// thing is not.
func TestTheDeploymentSaysWhichHealthModeItWires(t *testing.T) {
	compose, err := os.ReadFile(repoPath("deploy", "compose", "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest yaml.Node
	if err := yaml.Unmarshal(compose, &manifest); err != nil {
		t.Fatal(err)
	}

	modes := map[string]bool{}
	for _, test := range valuesOf(&manifest, "test") {
		for _, mode := range []string{"liveness", "readiness"} {
			if strings.Contains(test, "--mode="+mode) {
				modes[mode] = true
			}
		}
	}
	if len(modes) == 0 {
		t.Fatal("the reference manifest wires no health mode, so this proves nothing")
	}

	guide, err := os.ReadFile(repoPath("docs", "deployment.md"))
	if err != nil {
		t.Fatal(err)
	}
	// The checklist item is the sentence naming the healthcheck command.
	for _, mode := range []string{"liveness", "readiness"} {
		named := strings.Contains(string(guide), "`"+mode+"` only")
		if wired := modes[mode]; named && !wired {
			t.Errorf("deployment.md says the reference asks for %q only, and compose.yaml does not wire it", mode)
		}
	}
	if modes["readiness"] && strings.Contains(string(guide), "`liveness` only") {
		t.Error("deployment.md says the reference asks for liveness only; compose.yaml also wires readiness")
	}
}

// registryCommand matches a command that reaches a registry.
//
// The four that do: a build fetches its base image, a push and a pull
// speak to it by definition, and imagetools reads and writes manifests.
// `docker image inspect` is local and absent on purpose.
var registryCommand = regexp.MustCompile(`docker (build|push|pull) |docker buildx imagetools `)

// helperPath is the retry wrapper, named once because two tests
// reason about it: one about which commands go through it, and one
// about which jobs can find it.
const helperPath = "scripts/ci/retry.sh"

// TestEveryRegistryCommandIsRetried: four release cycles were lost to
// registries answering badly, and nothing asked twice.
//
// A digest verification that could not reach Docker Hub, a base image
// fetched through a 502, a module proxy that reset the stream mid-download,
// and a push GHCR accepted layer by layer and then called an unknown blob.
// None was a fault here, and each cost a maintainer a manual re-run of a
// tagged release — one of them after the tag had already been cut.
//
// The helper is bounded and blind: deciding which failures deserve another
// attempt means matching a registry's wording, which changes without
// telling anyone. What this holds is that a command reaching a registry
// goes through it, because the next one added is the one that will not.
func TestEveryRegistryCommandIsRetried(t *testing.T) {
	workflows, err := filepath.Glob(repoPath(".github", "workflows", "*.y*ml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(workflows) == 0 {
		t.Fatal("no workflow runs anything, so this proves nothing")
	}

	info, err := os.Stat(repoPath(helperPath))
	if err != nil {
		t.Fatalf("%s is missing, and every workflow calls it: %v", helperPath, err)
	}
	// Present is not enough. The workflows invoke it as a path, not
	// through an interpreter, so a helper that lost its execute bit --
	// which a checkout preserves and an editor or a patch can drop --
	// exists, reads correctly, and fails every step that calls it.
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("%s is not executable (mode %v); every step calling it would fail to start",
			helperPath, info.Mode().Perm())
	}

	found := 0
	for _, workflow := range workflows {
		body, err := os.ReadFile(workflow)
		if err != nil {
			t.Fatal(err)
		}
		var document yaml.Node
		if err := yaml.Unmarshal(body, &document); err != nil {
			t.Errorf("parse %s: %v", rel(workflow), err)
			continue
		}
		for _, script := range valuesOf(&document, "run") {
			for _, line := range strings.Split(script, "\n") {
				// Anywhere on the line, wrapped or not: a pattern that
				// only matched an unwrapped command would find none once
				// they all are, and its own vacuity guard would fire.
				// Comments name these commands while explaining them.
				if trimmed := strings.TrimSpace(line); !registryCommand.MatchString(line) ||
					strings.HasPrefix(trimmed, "#") {
					continue
				}
				found++
				if !strings.Contains(line, helperPath) {
					t.Errorf("%s reaches a registry without %s:\n    %s",
						rel(workflow), helperPath, strings.TrimSpace(line))
				}
			}
		}
	}
	if found == 0 {
		t.Fatal("no workflow reaches a registry, so this proves nothing")
	}
}

// TestEveryJobThatCallsTheHelperHasTheTreeItLivesIn.
//
// `scripts/ci/retry.sh` is a file in this repository, so a job that
// calls it and does not check the repository out gets exit 127 and a
// message about a missing file — not about the registry command it was
// wrapping.
//
// TestEveryRegistryCommandIsRetried cannot see this. It reads the
// workflows for commands that reach a registry and asks whether each
// one goes through the helper; whether the job around it has the helper
// on disk is a different question, and one no amount of reading the
// command answers.
//
// It has already cost a release. `capsule-index` assembles an index out
// of digests other jobs pushed, so it wanted nothing from the tree and
// checked nothing out — until the command it assembles them with was
// routed through a file that only exists there. The tag was cut, four
// jobs passed, and the fifth could not find a shell script.
func TestEveryJobThatCallsTheHelperHasTheTreeItLivesIn(t *testing.T) {
	workflows, err := filepath.Glob(repoPath(".github", "workflows", "*.y*ml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(workflows) == 0 {
		t.Fatal("no workflows found, so this proves nothing")
	}

	checked := 0
	for _, path := range workflows {
		var doc struct {
			Jobs map[string]struct {
				Steps []struct {
					Uses string `yaml:"uses"`
					Run  string `yaml:"run"`
				} `yaml:"steps"`
			} `yaml:"jobs"`
		}
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := yaml.Unmarshal(body, &doc); err != nil {
			t.Fatalf("%s: %v", filepath.Base(path), err)
		}
		for name, job := range doc.Jobs {
			calls, checksOut := false, false
			for _, step := range job.Steps {
				if strings.Contains(step.Run, helperPath) {
					calls = true
				}
				if strings.HasPrefix(step.Uses, "actions/checkout@") {
					checksOut = true
				}
			}
			if !calls {
				continue
			}
			checked++
			if !checksOut {
				t.Errorf("%s: job %q calls %s and checks nothing out. The helper is a file "+
					"in this repository, so the step exits 127 naming a missing script "+
					"rather than whatever it was wrapping",
					filepath.Base(path), name, helperPath)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no job calls the helper, so this proves nothing")
	}
	if !t.Failed() {
		t.Logf("%d jobs call %s, all with the tree", checked, helperPath)
	}
}

// TestEveryExternalWorkflowActionIsPinnedToACommit includes the deployment
// example because it is copied into repositories that may execute untrusted
// pull requests. A moving major tag lets an action publisher replace code
// without any change to the consuming repository; a full commit SHA makes the
// update visible and reviewable. Local reusable workflows are repository code
// already bound to the checked-out commit and therefore need no external ref.
func TestEveryExternalWorkflowActionIsPinnedToACommit(t *testing.T) {
	paths, err := filepath.Glob(repoPath(".github", "workflows", "*.y*ml"))
	if err != nil {
		t.Fatal(err)
	}
	examples, err := filepath.Glob(repoPath("deploy", "workflows", "*.y*ml"))
	if err != nil {
		t.Fatal(err)
	}
	paths = append(paths, examples...)
	if len(paths) == 0 {
		t.Fatal("no workflows found, so this proves nothing")
	}

	commit := regexp.MustCompile(`^[0-9a-f]{40}$`)
	checked := 0
	for _, path := range paths {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var document yaml.Node
		if err := yaml.Unmarshal(body, &document); err != nil {
			t.Fatalf("%s: %v", filepath.Base(path), err)
		}
		var inspect func(*yaml.Node)
		inspect = func(node *yaml.Node) {
			if node.Kind == yaml.MappingNode {
				for i := 0; i+1 < len(node.Content); i += 2 {
					key, value := node.Content[i], node.Content[i+1]
					if key.Value == "uses" && value.Kind == yaml.ScalarNode {
						ref := value.Value
						if strings.HasPrefix(ref, "./") {
							continue
						}
						checked++
						_, revision, ok := strings.Cut(ref, "@")
						if !ok || !commit.MatchString(revision) {
							t.Errorf("%s pins external action %q without a full commit SHA",
								filepath.Base(path), ref)
						}
					}
				}
			}
			for _, child := range node.Content {
				inspect(child)
			}
		}
		inspect(&document)
	}
	if checked == 0 {
		t.Fatal("no external workflow actions found, so this proves nothing")
	}
}
