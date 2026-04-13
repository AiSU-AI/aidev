package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// Comment is the subset of the GitHub issue-comment payload the controller
// cares about.
type Comment struct {
	ID   int64
	Body string
	URL  string
}

// PostComment adds a new comment to an issue and returns the created
// Comment. aidev's controller uses this for one-shot artifact comments
// (scout brief, critic report, sketches) that should survive untouched for
// the rest of the run.
func (c *Client) PostComment(ctx context.Context, owner, repo string, issue int, body string) (*Comment, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/issues/%d/comments", c.baseURL, owner, repo, issue)
	payload, err := json.Marshal(struct {
		Body string `json:"body"`
	}{Body: body})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	c.setAuth(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("post comment: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("post comment: %s", resp.Status)
	}
	var raw struct {
		ID      int64  `json:"id"`
		Body    string `json:"body"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode comment: %w", err)
	}
	return &Comment{ID: raw.ID, Body: raw.Body, URL: raw.HTMLURL}, nil
}

// UpdateComment replaces the body of an existing comment. The controller
// uses this for the pinned status comment that is rewritten in place at
// every state transition.
func (c *Client) UpdateComment(ctx context.Context, owner, repo string, commentID int64, body string) error {
	url := fmt.Sprintf("%s/repos/%s/%s/issues/comments/%d", c.baseURL, owner, repo, commentID)
	payload, err := json.Marshal(struct {
		Body string `json:"body"`
	}{Body: body})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	c.setAuth(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("update comment: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("update comment: %s", resp.Status)
	}
	return nil
}

// ListComments returns every comment on the issue. The controller uses
// this on startup to find its own prior comments (via fingerprint) so that
// re-runs update existing comments instead of duplicating them.
func (c *Client) ListComments(ctx context.Context, owner, repo string, issue int) ([]Comment, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/issues/%d/comments?per_page=100", c.baseURL, owner, repo, issue)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	c.setAuth(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list comments: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("list comments: %s", resp.Status)
	}
	var raw []struct {
		ID      int64  `json:"id"`
		Body    string `json:"body"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode comments: %w", err)
	}
	out := make([]Comment, 0, len(raw))
	for _, r := range raw {
		out = append(out, Comment{ID: r.ID, Body: r.Body, URL: r.HTMLURL})
	}
	return out, nil
}

// AddLabels adds one or more labels to an issue. Existing labels are
// preserved. Missing labels are created on demand by GitHub (with a
// default colour), which is fine for aidev's `aidev:<phase>` label set.
func (c *Client) AddLabels(ctx context.Context, owner, repo string, issue int, labels ...string) error {
	if len(labels) == 0 {
		return nil
	}
	url := fmt.Sprintf("%s/repos/%s/%s/issues/%d/labels", c.baseURL, owner, repo, issue)
	payload, err := json.Marshal(struct {
		Labels []string `json:"labels"`
	}{Labels: labels})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	c.setAuth(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("add labels: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("add labels: %s", resp.Status)
	}
	return nil
}

// CreateIssue opens a new issue on owner/repo with the given title,
// body, and labels. Used by the `aidev followups --file-issues`
// subcommand to file Reviewer-proposed follow-ups as real issues.
// Returns the created issue number and HTML URL.
func (c *Client) CreateIssue(ctx context.Context, owner, repo, title, body string, labels []string) (number int, url string, err error) {
	endpoint := fmt.Sprintf("%s/repos/%s/%s/issues", c.baseURL, owner, repo)
	payload := struct {
		Title  string   `json:"title"`
		Body   string   `json:"body"`
		Labels []string `json:"labels,omitempty"`
	}{Title: title, Body: body, Labels: labels}
	data, err := json.Marshal(payload)
	if err != nil {
		return 0, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return 0, "", err
	}
	c.setAuth(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, "", fmt.Errorf("create issue: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return 0, "", fmt.Errorf("create issue: %s", resp.Status)
	}
	var raw struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return 0, "", fmt.Errorf("create issue: decode: %w", err)
	}
	return raw.Number, raw.HTMLURL, nil
}

// RemoveLabel removes a single label from an issue. Absent labels are a
// no-op (we swallow the 404), so the controller can call this blindly
// when swapping phase labels.
func (c *Client) RemoveLabel(ctx context.Context, owner, repo string, issue int, label string) error {
	url := fmt.Sprintf("%s/repos/%s/%s/issues/%d/labels/%s", c.baseURL, owner, repo, issue, label)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return err
	}
	c.setAuth(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("remove label: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("remove label: %s", resp.Status)
	}
	return nil
}

// setAuth adds the standard GitHub headers to a request, including the
// token when one is available. Factored out so every write method agrees
// on headers without duplicating the logic.
func (c *Client) setAuth(req *http.Request) {
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "aidev")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
}
