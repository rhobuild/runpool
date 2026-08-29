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
// about the fixture organisation, not about this tree: identity and the
// organization cache probe both targets, and label routing dispatches
// into the first only.
var installedFixtures = map[string][]string{
	"identity.yml":           {envRepoA, envRepoB},
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
func TestEveryFixtureThisSuiteReadsIsInstalled(t *testing.T) {
	_, token := target(t, envOrgURL, envOrgToken)
	rest := newRESTClient(token)
	ctx := testCtx(t)

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
		repos, mapped := installedFixtures[name]
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

		for _, which := range repos {
			repo := os.Getenv(which)
			if repo == "" {
				if os.Getenv(envQualify) != "" {
					t.Fatalf("release qualification requires %s; the fixtures it names "+
						"cannot be checked", which)
				}
				t.Skipf("%s not set; the fixture check is opt-in", which)
			}
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
