package githubcontract

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// restClient is the minimal GitHub REST surface the harness needs:
// dispatch a workflow, find the run that dispatch created, cancel
// exactly that run, and read the installed fixture for parity. It exists
// so the suite depends on one explicit token instead of whatever
// authentication a `gh` binary happens to carry in its environment —
// ambient credentials are how a test quietly acts as the wrong identity.
type restClient struct {
	token string
	http  *http.Client
}

func newRESTClient(token string) *restClient {
	return &restClient{token: token, http: &http.Client{Timeout: 30 * time.Second}}
}

func (c *restClient) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://api.github.com"+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(payload)))
	}
	if out != nil {
		return json.Unmarshal(payload, out)
	}
	return nil
}

// dispatchWorkflow starts one run of the workflow with the given inputs.
// The API returns no run id; the caller correlates through the run name.
func (c *restClient) dispatchWorkflow(ctx context.Context, repo, workflow string, inputs map[string]string) error {
	return c.do(ctx, http.MethodPost,
		"/repos/"+repo+"/actions/workflows/"+url.PathEscape(workflow)+"/dispatches",
		map[string]any{"ref": "main", "inputs": inputs}, nil)
}

type workflowRun struct {
	ID           int64  `json:"id"`
	DisplayTitle string `json:"display_title"`
	Status       string `json:"status"`
	Conclusion   string `json:"conclusion"`
}

// findRunByCorrelation locates the single run whose display title
// carries the correlation id — the id this suite generated for exactly
// one dispatch — polling briefly because run creation is asynchronous.
func (c *restClient) findRunByCorrelation(ctx context.Context, repo, workflow, correlationID string, within time.Duration) (workflowRun, error) {
	deadline := time.Now().Add(within)
	for {
		var page struct {
			WorkflowRuns []workflowRun `json:"workflow_runs"`
		}
		err := c.do(ctx, http.MethodGet,
			"/repos/"+repo+"/actions/workflows/"+url.PathEscape(workflow)+"/runs?per_page=20", nil, &page)
		if err != nil {
			return workflowRun{}, err
		}
		for _, run := range page.WorkflowRuns {
			if strings.Contains(run.DisplayTitle, correlationID) {
				return run, nil
			}
		}
		if time.Now().After(deadline) {
			return workflowRun{}, fmt.Errorf("no run of %s carries correlation id %s within %s", workflow, correlationID, within)
		}
		select {
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
			return workflowRun{}, ctx.Err()
		}
	}
}

// cancelRun cancels one run by id. Cancelling by id is the whole point:
// a cleanup that sweeps every queued run of the workflow can cancel work
// belonging to a concurrent suite execution.
func (c *restClient) cancelRun(ctx context.Context, repo string, runID int64) error {
	err := c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/actions/runs/%d/cancel", repo, runID), nil, nil)
	// 409 means the run already finished, which is the goal state.
	if err != nil && strings.Contains(err.Error(), "409") {
		return nil
	}
	return err
}

// workflowRun reads one run by id.
func (c *restClient) workflowRun(ctx context.Context, repo string, runID int64) (workflowRun, error) {
	var run workflowRun
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/actions/runs/%d", repo, runID), nil, &run)
	return run, err
}

// installedFixtureDigest fetches the workflow file as installed in the
// fixture repository and returns its sha256.
func (c *restClient) installedFixtureDigest(ctx context.Context, repo, path string) (string, error) {
	var content struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	if err := c.do(ctx, http.MethodGet, "/repos/"+repo+"/contents/"+path, nil, &content); err != nil {
		return "", err
	}
	if content.Encoding != "base64" {
		return "", fmt.Errorf("contents API returned encoding %q; expected base64", content.Encoding)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(content.Content, "\n", ""))
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}
