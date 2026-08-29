package githubcontract

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

type installedFixture struct {
	repositories     []string
	qualifiesRelease bool
}

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
var installedFixtures = map[string]installedFixture{
	"identity.yml":           {repositories: []string{envRepoA}, qualifiesRelease: true},
	"organization-cache.yml": {repositories: []string{envRepoA, envRepoB}, qualifiesRelease: true},
	"label-routing.yml":      {repositories: []string{envRepoA}},
}

// TestEveryContractFixtureIsInstalled fails in seconds, naming every
// release-required fixture that is missing or has drifted before a contract
// dispatches. Observation-only fixtures are mapped here for parity but do not
// qualify a release.
//
// Each dispatching test verifies its own fixture again. This preflight reports
// all missing or drifted contract fixtures before the longer suite starts. The
// controller end-to-end fixture has separate credentials and its own parity
// check.
func TestEveryContractFixtureIsInstalled(t *testing.T) {
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
		fixture, mapped := installedFixtures[name]
		if !mapped {
			t.Errorf("testdata/%s is versioned here and no test says which repository "+
				"carries it. A fixture nothing installs is one a suite skips or fails "+
				"on, and neither says so until it runs", name)
			continue
		}
		if !fixture.qualifiesRelease {
			continue
		}
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		want := sha256.Sum256(body)

		for _, which := range fixture.repositories {
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
	t.Logf("%d installed contract fixtures match their versioned files", checked)
}
