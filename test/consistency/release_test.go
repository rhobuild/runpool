package consistency

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestTheReleaseBuildsEveryPlatformTheLockDeclares: the lock says what a
// release builds for, and the release workflow is what builds it.
//
// Nothing else relates the two. `build/images.lock.json` declares the
// platforms, `internal/app` embeds that declaration, and a Go test holds
// it equal to the platforms the pinned base images publish — but the
// workflow that produces the artifacts reads none of it. A lock
// declaring two platforms beside a release that pushes one is a claim
// with nothing behind it, and the first reader to notice is an operator
// pulling an image for an architecture that was never published.
func TestTheReleaseBuildsEveryPlatformTheLockDeclares(t *testing.T) {
	var lock struct {
		Platforms []string `json:"platforms"`
	}
	if err := readRepoJSON(t, "build/images.lock.json", &lock); err != nil {
		t.Fatal(err)
	}
	if len(lock.Platforms) == 0 {
		t.Fatal("the image lock declares no platform, so this proves nothing")
	}

	body, err := os.ReadFile(repoPath(".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow yaml.Node
	if err := yaml.Unmarshal(body, &workflow); err != nil {
		t.Fatal(err)
	}
	declared := slices.Clone(lock.Platforms)
	slices.Sort(declared)

	// Per matrix, not a union across the workflow: a union stays whole
	// while one job loses a leg — the capsule matrix down to one
	// architecture still contributed both values through the controller's
	// matrix, and the index published without the missing child. Each
	// matrix that names platforms must name every declared one itself.
	matrices := platformMatrices(&workflow)
	if len(matrices) < 2 {
		t.Fatalf("found %d platform matrices in release.yml; the capsule and controller "+
			"builds each carry one, so this proves nothing", len(matrices))
	}
	for i, built := range matrices {
		slices.Sort(built)
		built = slices.Compact(built)
		if !slices.Equal(built, declared) {
			t.Errorf("platform matrix %d builds for %s; the lock declares %s. A platform in one "+
				"list and not the other is one a release claims and does not produce, or one it "+
				"produces and does not offer.",
				i+1, strings.Join(built, ", "), strings.Join(declared, ", "))
		}
	}
}

// TestTheReleaseAsksWhetherTheChangelogNamesTheTag: the step is the only
// place a tag is read against the tree, and a workflow it was deleted
// from looks exactly like one that never carried it.
//
// The suite cannot ask the question itself — it never sees a tag — so a
// release could publish a changelog whose newest section names another
// version, or none, with every gate green.
//
// Where it is asked is half of it. A step that asks in a job nothing
// depends on runs, fails, and publishes anyway: the failure is one red
// square beside a release that shipped. So the job holding it has to be
// one the publishing job descends from.
func TestTheReleaseAsksWhetherTheChangelogNamesTheTag(t *testing.T) {
	const publishes = "publish"

	var workflow struct {
		Jobs map[string]struct {
			Needs yaml.Node `yaml:"needs"`
			Steps []struct {
				Run string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	body, err := os.ReadFile(repoPath(".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(body, &workflow); err != nil {
		t.Fatal(err)
	}

	var asking []string
	for name, job := range workflow.Jobs {
		for _, step := range job.Steps {
			if strings.Contains(step.Run, "CHANGELOG.md") && strings.Contains(step.Run, "VERSION") {
				asking = append(asking, name)
			}
		}
	}
	if len(asking) != 1 {
		t.Fatalf("%d steps in release.yml read CHANGELOG.md against the version, in %v; "+
			"exactly one does", len(asking), asking)
	}

	// Every job the publication transitively waits for.
	before := map[string]bool{}
	var walk func(string)
	walk = func(name string) {
		for _, need := range needsOf(workflow.Jobs[name].Needs) {
			if !before[need] {
				before[need] = true
				walk(need)
			}
		}
	}
	walk(publishes)
	if len(before) == 0 {
		t.Fatalf("%q waits for nothing, so this proves nothing", publishes)
	}
	if !before[asking[0]] {
		t.Errorf("the changelog is read in %q, which %q does not wait for; the step would "+
			"fail beside a release that published anyway", asking[0], publishes)
	}
}

// needsOf reads a job's dependencies, which the workflow syntax writes as
// one name or as a list of them.
func needsOf(node yaml.Node) []string {
	switch node.Kind {
	case yaml.ScalarNode:
		return []string{node.Value}
	case yaml.SequenceNode:
		var names []string
		for _, item := range node.Content {
			names = append(names, item.Value)
		}
		return names
	}
	return nil
}

// platformMatrices returns, for each strategy matrix in the document,
// the platform values its include entries carry — one slice per matrix,
// so a leg missing from one cannot be papered over by its sibling.
func platformMatrices(n *yaml.Node) [][]string {
	var out [][]string
	var walk func(*yaml.Node)
	walk = func(n *yaml.Node) {
		if n.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(n.Content); i += 2 {
				if n.Content[i].Value == "matrix" {
					if built := valuesOf(n.Content[i+1], "platform"); len(built) > 0 {
						out = append(out, built)
					}
				}
				walk(n.Content[i+1])
			}
			return
		}
		for _, c := range n.Content {
			walk(c)
		}
	}
	walk(n)
	return out
}

// TestTheReleaseBodyCarriesTheChangelog: a release page is where the
// version is read from, and for most readers it is the only page they
// will open. A body carrying provenance and nothing else answers "what
// exactly is this" and never answers "what changed", which is what the
// reader came for.
//
// The failure this pins is silent, which is why it needs pinning: an
// expression reaching a job the publishing job does not directly depend
// on is not an error in GitHub Actions. It is the empty string. Drop
// `validate` from the needs list and every gate stays green, the release
// publishes, and the section is simply gone -- with the workflow still
// reading exactly as though it carried one.
func TestTheReleaseBodyCarriesTheChangelog(t *testing.T) {
	var workflow struct {
		Jobs map[string]struct {
			Needs yaml.Node `yaml:"needs"`
			Steps []struct {
				Uses string            `yaml:"uses"`
				With map[string]string `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	body, err := os.ReadFile(repoPath(".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(body, &workflow); err != nil {
		t.Fatal(err)
	}

	var bodies []string
	var publishing string
	for name, job := range workflow.Jobs {
		for _, step := range job.Steps {
			if !strings.HasPrefix(step.Uses, "softprops/action-gh-release@") {
				continue
			}
			bodies = append(bodies, step.With["body"])
			publishing = name
		}
	}
	if len(bodies) != 1 {
		t.Fatalf("%d steps publish a GitHub release; exactly one does", len(bodies))
	}

	// The changelog travels as job outputs. Which job produced them is
	// read from the expression rather than assumed, so the reachability
	// check below is against the job the body actually names.
	source := ""
	for _, field := range []string{"lead", "detail"} {
		want := "needs.NAME.outputs." + field
		found := ""
		for name := range workflow.Jobs {
			if strings.Contains(bodies[0], strings.Replace(want, "NAME", name, 1)) {
				found = name
			}
		}
		if found == "" {
			t.Fatalf("the release body interpolates no %s output from any job; "+
				"the page would publish without the changelog's %s", field, field)
		}
		if source != "" && found != source {
			t.Fatalf("the release body takes lead from %q and detail from %q; "+
				"one section split across two readings is two sections", source, found)
		}
		source = found
	}

	if !slices.Contains(needsOf(workflow.Jobs[publishing].Needs), source) {
		t.Errorf("the release body reads outputs of %q, which %q does not directly "+
			"depend on; the expression resolves to the empty string and the release "+
			"publishes with no changelog and no error", source, publishing)
	}
}

// TestTheNewestChangelogSectionCouldBeReleased runs the shape half of
// the release gate against the real file, on every pull request.
//
// The gate itself lives in release.yml and can only run on a tag,
// because the half it exists for compares the newest heading against the
// version being released and a version exists only there. But the rest
// of what it demands has nothing to do with a version: a section needs
// an opening summary, its links have to be ones the workflow knows how
// to pin, and it must not contain the delimiter that carries it to the
// release body.
//
// None of that was checked anywhere until a tag was pushed — which is
// the most expensive place to find it, because the tag is protected by a
// ruleset with no bypass actors and the failure lands after it exists.
// It has already happened once: the section that became v1.2.0 opened
// straight onto a group heading, and the gate refused it.
func TestTheNewestChangelogSectionCouldBeReleased(t *testing.T) {
	body, err := os.ReadFile(repoPath("CHANGELOG.md"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(body), "\n")

	start := -1
	for i, line := range lines {
		if strings.HasPrefix(line, "## ") {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatal("CHANGELOG.md has no section heading at all, so this proves nothing")
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "## ") {
			end = i
			break
		}
	}
	heading, section := lines[start], lines[start+1:end]

	// The lead is everything before the first group, and the workflow
	// refuses a section without one: a release page that opens on a
	// bullet list says what changed and never says what this is.
	var lead []string
	for _, line := range section {
		if strings.HasPrefix(line, "### ") {
			break
		}
		lead = append(lead, line)
	}
	if strings.TrimSpace(strings.Join(lead, "")) == "" {
		t.Errorf("the newest section %q has no summary paragraph under it. The release "+
			"gate refuses that, on the tag, after it exists", heading)
	}

	// Every relative link has to start with a directory the workflow's
	// pinning knows, or it reaches the release page unresolvable.
	pinnable := regexp.MustCompile(`^(docs|build|deploy|scripts|test|internal|cmd|\.github)/`)
	link := regexp.MustCompile(`\]\(([^)]*)\)`)
	for i, line := range section {
		for _, m := range link.FindAllStringSubmatch(line, -1) {
			target := m[1]
			if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") ||
				strings.HasPrefix(target, "#") || pinnable.MatchString(target) {
				continue
			}
			t.Errorf("CHANGELOG.md:%d links to %q, which the release workflow cannot pin "+
				"to a tag; it would publish as a broken link or fail the gate",
				start+2+i, target)
		}
	}

	// The heredoc delimiter the workflow carries this section with. A
	// line equal to it would end the value early and leave the rest
	// parsed as further outputs.
	const delimiter = "RUNPOOL_CHANGELOG_SECTION"
	for i, line := range section {
		if line == delimiter {
			t.Errorf("CHANGELOG.md:%d is exactly the delimiter the release workflow "+
				"writes this section with", start+2+i)
		}
	}
}
