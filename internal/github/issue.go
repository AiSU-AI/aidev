// Package github fetches issue bodies from github.com over the REST API.
//
// v0.1 is read-only: we only need to hydrate an issue given either a URL or
// an owner/repo/number triple. Writing back (comments, PR creation) will
// land when the Implementer agent comes online.
package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"time"
)

// Issue is the subset of GitHub's issue payload aidev actually uses.
type Issue struct {
	Owner  string
	Repo   string
	Number int
	Title  string
	Body   string
	State  string
	URL    string
	Labels []string
}

// defaultAPIBase is the production GitHub API endpoint. Tests inject a
// fake one via NewClientWithEndpoint.
const defaultAPIBase = "https://api.github.com"

// Client is a REST client for reading and writing issues and their
// comments/labels. Auth is optional for public repo reads — but rate
// limits are much friendlier when GITHUB_TOKEN is set, and private repos
// and all writes require it.
type Client struct {
	token   string
	baseURL string // no trailing slash
	http    *http.Client
}

// NewClient constructs a Client against the real github.com API, reading
// the token from GITHUB_TOKEN.
func NewClient() *Client {
	return &Client{
		token:   os.Getenv("GITHUB_TOKEN"),
		baseURL: defaultAPIBase,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// NewClientWithEndpoint constructs a Client that talks to baseURL instead
// of api.github.com. Used by tests with httptest servers; also usable for
// GitHub Enterprise installations in the future. baseURL may have a
// trailing slash or not.
func NewClientWithEndpoint(token, baseURL string) *Client {
	if baseURL == "" {
		baseURL = defaultAPIBase
	}
	for len(baseURL) > 0 && baseURL[len(baseURL)-1] == '/' {
		baseURL = baseURL[:len(baseURL)-1]
	}
	return &Client{
		token:   token,
		baseURL: baseURL,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

var issueURLRe = regexp.MustCompile(`^https?://github\.com/([^/]+)/([^/]+)/issues/(\d+)`)

// ParseURL extracts owner/repo/number from a github.com issue URL. An empty
// URL returns a helpful error rather than a zero struct.
func ParseURL(url string) (owner, repo string, number int, err error) {
	if url == "" {
		return "", "", 0, errors.New("github: empty issue URL")
	}
	m := issueURLRe.FindStringSubmatch(url)
	if m == nil {
		return "", "", 0, fmt.Errorf("github: not an issue URL: %s", url)
	}
	n, err := strconv.Atoi(m[3])
	if err != nil {
		return "", "", 0, err
	}
	return m[1], m[2], n, nil
}

// Fetch returns the issue identified by owner/repo/number.
func (c *Client) Fetch(ctx context.Context, owner, repo string, number int) (*Issue, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/issues/%d", c.baseURL, owner, repo, number)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("github: issue %s/%s#%d not found (private repo? set GITHUB_TOKEN)", owner, repo, number)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("github: %s", resp.Status)
	}

	var raw struct {
		HTMLURL string `json:"html_url"`
		Number  int    `json:"number"`
		Title   string `json:"title"`
		Body    string `json:"body"`
		State   string `json:"state"`
		Labels  []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("github decode: %w", err)
	}

	labels := make([]string, 0, len(raw.Labels))
	for _, l := range raw.Labels {
		labels = append(labels, l.Name)
	}

	return &Issue{
		Owner:  owner,
		Repo:   repo,
		Number: raw.Number,
		Title:  raw.Title,
		Body:   raw.Body,
		State:  raw.State,
		URL:    raw.HTMLURL,
		Labels: labels,
	}, nil
}
