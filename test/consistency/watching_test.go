package consistency

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rhobuild/runpool/internal/config"
)

// TestTheWatchingThresholdsFollowTheConstantsTheyAreDerivedFrom.
//
// The runbook's watching section tells an operator to alert on a lease
// that has been resting for thirty minutes, on one that has outlived a
// hundred and sixty-nine hours, and on a disk measurement fifteen
// minutes old. None of those numbers is arbitrary: each is a multiple of
// a constant in the code, and the section says which. Nothing tied them
// together.
//
// This is the shape this package exists for. Change
// capsulePrepTimeout and the thirty minutes is no longer twice the
// longest a healthy capsule takes to prepare -- it is a number that used
// to mean that. The document keeps saying it does, the tests stay green,
// and the first reader to find out is an operator whose alert fires on
// every healthy job, or one whose alert never fires at all.
func TestTheWatchingThresholdsFollowTheConstantsTheyAreDerivedFrom(t *testing.T) {
	section := watchingSection(t)

	// Counted, not searched. Two of these work out to the same number
	// today -- twice a capsule's preparation and six rediscovery
	// intervals are both thirty minutes -- so asking whether the section
	// contains 1800 is answered by either of them. Delete one line and
	// the other still answers for it.
	present := map[int64]int{}
	for _, m := range regexp.MustCompile(`> (\d+)\)`).FindAllStringSubmatch(section, -1) {
		n, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		present[n]++
	}

	for _, tc := range []struct {
		name string
		want time.Duration
		why  string
	}{{
		name: "a lease resting, at twice the longest a preparation may take",
		want: 2 * sourceDuration(t, "internal/app/serve.go", "capsulePrepTimeout"),
		why:  "nothing healthy sits outside workload_running longer than a preparation can take",
	}, {
		name: "a lease past any permitted ceiling, at the validator's maximum plus an hour",
		want: time.Duration(config.MaxJobTimeout) + time.Hour,
		why: "a lease is created before its job and outlives it, so a bound at exactly the " +
			"maximum fires on a job that ran its full ceiling",
	}, {
		name: "a disk measurement, at fifteen times the interval it is taken on",
		want: 15 * sourceDuration(t, "internal/app/monitor.go", "monitorInterval"),
		why:  "the level is only rewritten by a measurement that succeeded",
	}, {
		name: "a sandbox pass, at six times the interval it runs on",
		want: 6 * sourceDuration(t, "internal/netsandbox/netsandbox.go", "rediscoverInterval"),
		why:  "no healthy instance misses six rediscoveries",
	}} {
		seconds := int64(tc.want.Seconds())
		if present[seconds] == 0 {
			t.Errorf("the watching section has no threshold of %d seconds (%s) left, which is "+
				"what %q works out to today. Either a constant moved and the document did not, "+
				"or the derivation changed and this test did not: %s",
				seconds, tc.want, tc.name, tc.why)
			continue
		}
		// Spent, so a second condition deriving the same number has to
		// find its own occurrence rather than this one.
		present[seconds]--
	}
}

// watchingSection is the runbook's watching section, which is where the
// thresholds live.
func watchingSection(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(repoPath("docs", "runbook.md"))
	if err != nil {
		t.Fatal(err)
	}
	const heading = "## Watching an instance"
	start := strings.Index(string(body), heading)
	if start < 0 {
		t.Fatalf("docs/runbook.md has no %q section, so this proves nothing", heading)
	}
	rest := string(body)[start+len(heading):]
	if end := strings.Index(rest, "\n## "); end >= 0 {
		rest = rest[:end]
	}
	return rest
}

// sourceDuration reads a duration constant out of the file that declares
// it. The constants are unexported and this package may not import the
// ones that matter here, so it reads the declaration rather than the
// value -- which is enough, because what drifts is the number, and a
// declaration this cannot parse fails rather than passing.
func sourceDuration(t *testing.T, path, name string) time.Duration {
	t.Helper()
	body, err := os.ReadFile(repoPath(strings.Split(path, "/")...))
	if err != nil {
		t.Fatal(err)
	}
	// Either inside a const block or a `const name = ...` of its own.
	re := regexp.MustCompile(`(?m)^\s*(?:const\s+)?` + regexp.QuoteMeta(name) +
		`\s*=\s*(?:(\d+)\s*\*\s*)?time\.(Second|Minute|Hour)\b`)
	m := re.FindStringSubmatch(string(body))
	if m == nil {
		t.Fatalf("%s declares no duration constant %q this test can read; it was renamed, "+
			"moved, or written in a form this does not parse, and the thresholds derived "+
			"from it are now unchecked", path, name)
	}
	n := int64(1)
	if m[1] != "" {
		if n, err = strconv.ParseInt(m[1], 10, 64); err != nil {
			t.Fatal(err)
		}
	}
	switch m[2] {
	case "Second":
		return time.Duration(n) * time.Second
	case "Minute":
		return time.Duration(n) * time.Minute
	default:
		return time.Duration(n) * time.Hour
	}
}
