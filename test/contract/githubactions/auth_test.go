package githubcontract

import (
	"testing"

	"github.com/actions/scaleset"
)

// TestInvalidCredentialFailsClosed verifies that authentication fails before
// any resource is created.
func TestInvalidCredentialFailsClosed(t *testing.T) {
	url, _ := target(t, envRepoURL, envRepoToken)

	c, err := scaleset.NewClientWithPersonalAccessToken(scaleset.NewClientWithPersonalAccessTokenConfig{
		GitHubConfigURL:     url,
		PersonalAccessToken: "runpool-ct-invalid-token",
		SystemInfo:          scaleset.SystemInfo{System: "runpool", Subsystem: "contract-test"},
	})
	if err != nil {
		// Construction-time rejection is also fail-closed.
		t.Logf("client construction rejected invalid token: %v", err)
		return
	}
	if _, err := c.GetRunnerGroupByName(testCtx(t), scaleset.DefaultRunnerGroup); err == nil {
		t.Fatal("invalid token resolved a runner group; want authentication failure")
	} else {
		t.Logf("failed closed: %v", err)
	}
}
