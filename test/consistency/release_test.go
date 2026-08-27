package consistency

import (
	"os"
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
