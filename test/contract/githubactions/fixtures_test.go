package githubcontract

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// installedFixtures maps each versioned fixture to the repositories that
// must carry it, by the environment variable naming each.
//
// It is written out rather than derived because the mapping is a fact
// about the fixture organisation, not about this tree: the organization
// cache probe is the only one that needs both targets, because what it
// proves is that a JIT runner is not bound to the repository that caused
// it. Identity and label routing dispatch into the first alone.
//
// An entry claiming more than a test needs is worse than no check at
// all: it fails a qualification for a fixture nothing reads, which
// nothing keeps in sync, for a gap that does not exist.
var installedFixtures = map[string][]string{
	"identity.yml":           {envRepoA},
	"organization-cache.yml": {envRepoA, envRepoB},
	"label-routing.yml":      {envRepoA},
}

// TestEveryFixtureThisSuiteReadsIsInstalled fails in seconds, naming
// every fixture that is missing or has drifted, before anything
// dispatches.
//
// Each test that uses a fixture already verifies its own copy — that is
// the check that must never be removed, because it runs against the file
// the dispatch is about to use. This one exists for when it fails: a
// missing fixture surfaced eight minutes into a release, from one test,
// naming one file, after the tag had been cut. The same absence is
// knowable in five seconds and can name all of them at once.
//
// It is also the check a maintainer can run before cutting a tag, which
// is the point: the fixtures live in other repositories, and nothing in
// this one can otherwise say whether they are there.
//
// Scope, said out loud because the name does not say it: this covers the
// fixtures this package reads. The controller end-to-end suite keeps its
// own, in a third repository under a different filename and behind
// separate credentials, and it is not checked here. That gap is worse
// than the one this closes -- the e2e job runs after the live contracts,
// so a fixture missing there surfaces later than the one that cost this
// cycle, not earlier.
func TestEveryFixtureThisSuiteReadsIsInstalled(t *testing.T) {
	_, token := target(t, envOrgURL, envOrgToken)
	rest := newRESTClient(token)
	ctx := testCtx(t)

	// Resolved before the loop, and that is not tidiness: t.Skip runs
	// Goexit, so a skip decided inside the loop abandons every fixture
	// after it -- in a test whose whole promise is naming all of them at
	// once.
	repos := map[string]string{}
	for _, which := range []string{envRepoA, envRepoB} {
		repo := os.Getenv(which)
		if repo == "" {
			if os.Getenv(envQualify) != "" {
				t.Fatalf("release qualification requires %s; the fixtures it carries "+
					"cannot be checked", which)
			}
			t.Skipf("%s not set; the fixture check is opt-in", which)
		}
		repos[which] = repo
	}

	files, err := filepath.Glob(filepath.Join("testdata", "*.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no versioned fixtures found, so this proves nothing")
	}

	checked := 0
	for _, path := range files {
		name := filepath.Base(path)
		carriedBy, mapped := installedFixtures[name]
		if !mapped {
			t.Errorf("testdata/%s is versioned here and no test says which repository "+
				"carries it. A fixture nothing installs is one a suite skips or fails "+
				"on, and neither says so until it runs", name)
			continue
		}
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		want := sha256.Sum256(body)

		for _, which := range carriedBy {
			repo := repos[which]
			checked++
			got, err := rest.installedFixtureDigest(ctx, repo, ".github/workflows/"+name)
			if err != nil {
				t.Errorf("%s is not installed on %s: %v\n    install testdata/%s there, "+
					"byte for byte", name, repo, err, name)
				continue
			}
			if got != hex.EncodeToString(want[:]) {
				t.Errorf("%s on %s has drifted from testdata/%s; the suite would measure a "+
					"workflow this repository no longer describes", name, repo, name)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no fixture was checked against any repository, so this proves nothing")
	}
	t.Logf("%d installed fixtures match %s", checked, fmt.Sprintf("%d versioned files", len(files)))
}
