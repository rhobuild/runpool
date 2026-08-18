package controllere2e

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

type githubClient struct {
	baseURL    string
	repository string
	token      string
	http       *http.Client
}

type workflowRun struct {
	ID          int64     `json:"id"`
	DisplayName string    `json:"display_title"`
	Status      string    `json:"status"`
	Conclusion  string    `json:"conclusion"`
	HTMLURL     string    `json:"html_url"`
	CreatedAt   time.Time `json:"created_at"`
}

func newGitHubClient(s settings) *githubClient {
	return &githubClient{
		baseURL:    strings.TrimRight(s.apiURL, "/"),
		repository: s.repository,
		token:      s.token,
		http:       &http.Client{Timeout: 30 * time.Second},
	}
}

func (g *githubClient) verifyFixture(ctx context.Context, ref string, expected []byte) error {
	var content struct {
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
	}
	endpoint := fmt.Sprintf("/repos/%s/contents/.github/workflows/runpool-e2e.yml?ref=%s",
		g.repository, url.QueryEscape(ref))
	if err := g.request(ctx, http.MethodGet, endpoint, nil, &content); err != nil {
		return fmt.Errorf("read fixture workflow: %w", err)
	}
	if content.Encoding != "base64" {
		return fmt.Errorf("fixture workflow encoding is %q; want base64", content.Encoding)
	}
	actual, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(content.Content, "\n", ""))
	if err != nil {
		return fmt.Errorf("decode fixture workflow: %w", err)
	}
	if !bytes.Equal(bytes.TrimSpace(actual), bytes.TrimSpace(expected)) {
		return errors.New("fixture .github/workflows/runpool-e2e.yml differs from test/e2e/controller/testdata/workload.yml")
	}
	return nil
}

func (g *githubClient) dispatch(ctx context.Context, ref, runID, runnerLabel, cacheKey, expectation string, holdSeconds int) (workflowRun, error) {
	started := time.Now().UTC()
	payload := map[string]any{
		"ref": ref,
		"inputs": map[string]string{
			"run_id":            runID,
			"runner_label":      runnerLabel,
			"cache_key":         cacheKey,
			"cache_expectation": expectation,
			"hold_seconds":      fmt.Sprint(holdSeconds),
		},
	}
	endpoint := fmt.Sprintf("/repos/%s/actions/workflows/runpool-e2e.yml/dispatches", g.repository)
	if err := g.request(ctx, http.MethodPost, endpoint, payload, nil); err != nil {
		return workflowRun{}, fmt.Errorf("dispatch workflow: %w", err)
	}

	title := "runpool-e2e-" + runID
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		runs, err := g.listRuns(ctx)
		if err != nil {
			return workflowRun{}, err
		}
		for _, run := range runs {
			if run.DisplayName == title && !run.CreatedAt.Before(started.Add(-5*time.Second)) {
				return run, nil
			}
		}
		if err := wait(ctx, 2*time.Second); err != nil {
			return workflowRun{}, err
		}
	}
	return workflowRun{}, fmt.Errorf("dispatched workflow %q did not appear", title)
}

func (g *githubClient) listRuns(ctx context.Context) ([]workflowRun, error) {
	var response struct {
		Runs []workflowRun `json:"workflow_runs"`
	}
	endpoint := fmt.Sprintf("/repos/%s/actions/workflows/runpool-e2e.yml/runs?event=workflow_dispatch&per_page=30", g.repository)
	if err := g.request(ctx, http.MethodGet, endpoint, nil, &response); err != nil {
		return nil, fmt.Errorf("list workflow runs: %w", err)
	}
	return response.Runs, nil
}

func (g *githubClient) waitForStatus(ctx context.Context, run workflowRun, status string, timeout time.Duration) (workflowRun, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		current, err := g.getRun(ctx, run.ID)
		if err != nil {
			return workflowRun{}, err
		}
		if current.Status == status {
			return current, nil
		}
		if current.Status == "completed" {
			return current, fmt.Errorf("workflow completed as %s before reaching %s", current.Conclusion, status)
		}
		if err := wait(ctx, 3*time.Second); err != nil {
			return workflowRun{}, err
		}
	}
	return workflowRun{}, fmt.Errorf("workflow %d did not reach %s within %s", run.ID, status, timeout)
}

func (g *githubClient) waitForSuccess(ctx context.Context, run workflowRun, timeout time.Duration) (workflowRun, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		current, err := g.getRun(ctx, run.ID)
		if err != nil {
			return workflowRun{}, err
		}
		if current.Status == "completed" {
			if current.Conclusion != "success" {
				return current, fmt.Errorf("workflow %d concluded %q: %s", current.ID, current.Conclusion, current.HTMLURL)
			}
			return current, nil
		}
		if err := wait(ctx, 5*time.Second); err != nil {
			return workflowRun{}, err
		}
	}
	return workflowRun{}, fmt.Errorf("workflow %d did not complete within %s", run.ID, timeout)
}

func (g *githubClient) getRun(ctx context.Context, id int64) (workflowRun, error) {
	var run workflowRun
	endpoint := fmt.Sprintf("/repos/%s/actions/runs/%d", g.repository, id)
	if err := g.request(ctx, http.MethodGet, endpoint, nil, &run); err != nil {
		return run, fmt.Errorf("read workflow run %d: %w", id, err)
	}
	return run, nil
}

func (g *githubClient) cancel(ctx context.Context, id int64) error {
	endpoint := fmt.Sprintf("/repos/%s/actions/runs/%d/cancel", g.repository, id)
	err := g.request(ctx, http.MethodPost, endpoint, map[string]string{}, nil)
	if err != nil && !strings.Contains(err.Error(), "409") {
		return fmt.Errorf("cancel workflow run %d: %w", id, err)
	}
	return nil
}

func (g *githubClient) request(ctx context.Context, method, endpoint string, input, output any) error {
	var body io.Reader
	if input != nil {
		payload, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(payload)
	}
	if !strings.HasPrefix(endpoint, "/") {
		return errors.New("GitHub API endpoint must start with a slash")
	}
	req, err := http.NewRequestWithContext(ctx, method, g.baseURL+endpoint, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("GitHub API %s %s returned %d: %s", method, endpoint, resp.StatusCode, strings.TrimSpace(string(payload)))
	}
	if output != nil && len(payload) > 0 {
		if err := json.Unmarshal(payload, output); err != nil {
			return fmt.Errorf("decode GitHub API response: %w", err)
		}
	}
	return nil
}

func wait(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func TestGitHubRequestPreservesEscapedPathAndQuery(t *testing.T) {
	wantPath := "/orgs/acme/packages/container/repository%2Frunpool-e2e/versions"
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if got := r.URL.EscapedPath(); got != wantPath {
			t.Errorf("escaped path = %q; want %q", got, wantPath)
		}
		if got := r.URL.Query().Get("per_page"); got != "100" {
			t.Errorf("per_page = %q; want 100", got)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader("[]")),
		}, nil
	})

	client := &githubClient{
		baseURL: "https://api.github.test", token: "test",
		http: &http.Client{Transport: transport},
	}
	endpoint := fmt.Sprintf("/orgs/acme/packages/container/%s/versions?per_page=100",
		url.PathEscape("repository/runpool-e2e"))
	var response []any
	if err := client.request(t.Context(), http.MethodGet, endpoint, nil, &response); err != nil {
		t.Fatal(err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}
