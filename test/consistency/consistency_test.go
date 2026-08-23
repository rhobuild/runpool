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
		case !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go"):
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

// TestEveryReaderOfThePlatformLockKnowsItsShape: the lock is parsed
// outside Go too, and nothing there fails until a release.
//
// The release gate assembles its record with an inline reader. A change
// to the lock's shape leaves that reader compiling nothing and failing
// no test — it surfaces the first time somebody freezes an entry and
// cuts a candidate, and it surfaces as a claim about the reference that
// is not true of it.
//
// This is a text check, which is weaker than running the reader. Running
// it needs the reader to exist outside the workflow, and that is worth
// doing; until then this is what stands between a schema change and a
// release.
func TestEveryReaderOfThePlatformLockKnowsItsShape(t *testing.T) {
	raw, err := os.ReadFile(repoPath("build", "platform.lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	var lock map[string]json.RawMessage
	if err := json.Unmarshal(raw, &lock); err != nil {
		t.Fatal(err)
	}
	if _, ok := lock["platforms"]; !ok {
		t.Fatal("the lock has no `platforms` key, so this check is about a shape that is gone")
	}

	out, err := exec.Command("git", "-C", repoPath(), "ls-files",
		".github/workflows/*.yml", "scripts/*/*.sh", "scripts/*/*.py").Output()
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, file := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if file == "" {
			continue
		}
		body, err := os.ReadFile(repoPath(file))
		if err != nil {
			t.Fatal(err)
		}
		text := string(body)
		if !strings.Contains(text, "platform.lock.json") {
			continue
		}
		checked++
		// A reader that only names the file may just be pointing at it in
		// a comment; one that indexes into it has to know the shape.
		//
		// The forms below are the top-level ones the old shape had. A
		// bare path fragment cannot be on this list: an entry's policy is
		// still reached through `.policy.`, and its facts through
		// `.platform.`, so a correct reader of the list would be refused
		// by a check that reads it as the shape that is gone.
		for _, gone := range []string{`reference["policy"]`, `reference["platform"]`,
			`reference.get("status")`} {
			if strings.Contains(text, gone) {
				t.Errorf("%s reads the lock as %s, which the file has not had since it "+
					"became a list of platforms; this fails at release time, saying "+
					"something untrue about the reference", file, gone)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no file outside Go names the lock, so this proves nothing")
	}
}
