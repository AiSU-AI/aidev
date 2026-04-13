package orchestrator

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/aisu-ai/aidev/internal/agents"
	"github.com/aisu-ai/aidev/internal/github"
)

// fakeServer is a tiny httptest harness that mimics the handful of
// GitHub REST endpoints the GitHubReporter touches. It records every
// request so tests can assert on sequencing.
type fakeServer struct {
	t      *testing.T
	server *httptest.Server

	mu             sync.Mutex
	postComments   []string // bodies, in order
	patchComments  []patch
	listComments   []github.Comment
	addLabels      [][]string
	removeLabels   []string
	nextCommentID  int64
	statusFound    bool
}

type patch struct {
	id   int64
	body string
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	fs := &fakeServer{t: t, nextCommentID: 100}
	fs.server = httptest.NewServer(http.HandlerFunc(fs.serve))
	t.Cleanup(fs.server.Close)
	return fs
}

func (fs *fakeServer) serve(w http.ResponseWriter, r *http.Request) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	// All routes we implement.
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments"):
		// ListComments for the issue.
		if len(fs.listComments) == 0 {
			w.Write([]byte("[]"))
			return
		}
		var b strings.Builder
		b.WriteString("[")
		for i, c := range fs.listComments {
			if i > 0 {
				b.WriteString(",")
			}
			// Minimal shape the client decoder expects.
			b.WriteString(`{"id":`)
			b.WriteString(intToString(c.ID))
			b.WriteString(`,"body":`)
			b.WriteString(quote(c.Body))
			b.WriteString(`,"html_url":""}`)
		}
		b.WriteString("]")
		w.Write([]byte(b.String()))

	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
		// PostComment.
		body, _ := io.ReadAll(r.Body)
		fs.postComments = append(fs.postComments, string(body))
		fs.nextCommentID++
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id":` + intToString(fs.nextCommentID) + `,"body":"","html_url":""}`))

	case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/issues/comments/"):
		// UpdateComment.
		body, _ := io.ReadAll(r.Body)
		id := parseTrailingInt(r.URL.Path)
		fs.patchComments = append(fs.patchComments, patch{id: id, body: string(body)})
		w.WriteHeader(http.StatusOK)

	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/labels"):
		// AddLabels.
		body, _ := io.ReadAll(r.Body)
		fs.addLabels = append(fs.addLabels, extractStrings(string(body)))
		w.WriteHeader(http.StatusOK)

	case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/labels/"):
		// RemoveLabel. Extract the label name from the URL tail.
		idx := strings.Index(r.URL.Path, "/labels/")
		fs.removeLabels = append(fs.removeLabels, r.URL.Path[idx+len("/labels/"):])
		w.WriteHeader(http.StatusOK)

	default:
		fs.t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func TestGitHubReporterStatusCommentCreatedOnFirstEvent(t *testing.T) {
	fs := newFakeServer(t)
	client := github.NewClientWithEndpoint("tok", fs.server.URL+"/")
	issue := &github.Issue{Owner: "o", Repo: "r", Number: 1}
	rep := NewGitHubReporter(context.Background(), client, issue, io.Discard)

	err := rep.OnEvent(context.Background(), Event{State: StateScouting, Message: "starting scout"})
	if err != nil {
		t.Fatal(err)
	}

	if len(fs.postComments) != 1 {
		t.Fatalf("expected 1 post (status comment), got %d", len(fs.postComments))
	}
	body := fs.postComments[0]
	if !strings.Contains(body, "Scout running") {
		t.Errorf("status body missing headline: %q", body)
	}
	// The fingerprint is HTML (< and > characters); encoding/json escapes
	// them to \u003c / \u003e in the POST body. Check for the stable
	// "aidev:status" substring, which survives JSON escaping intact.
	if !strings.Contains(body, "aidev:status") {
		t.Errorf("status body missing fingerprint substring: %q", body)
	}
	if len(fs.addLabels) != 1 {
		t.Errorf("expected 1 label add, got %d: %v", len(fs.addLabels), fs.addLabels)
	}
}

func TestGitHubReporterUpdatesStatusInPlaceOnSubsequentEvents(t *testing.T) {
	fs := newFakeServer(t)
	client := github.NewClientWithEndpoint("tok", fs.server.URL+"/")
	issue := &github.Issue{Owner: "o", Repo: "r", Number: 1}
	rep := NewGitHubReporter(context.Background(), client, issue, io.Discard)
	ctx := context.Background()

	// Scouting — creates status comment
	if err := rep.OnEvent(ctx, Event{State: StateScouting, Message: "go"}); err != nil {
		t.Fatal(err)
	}
	// Critiquing — posts scout artifact + updates status + swaps label
	if err := rep.OnEvent(ctx, Event{State: StateCritiquing, Message: "go", Report: &agents.Context{ScoutReport: "a brief"}}); err != nil {
		t.Fatal(err)
	}

	if len(fs.postComments) != 2 {
		t.Errorf("expected 2 posts (status + scout artifact), got %d", len(fs.postComments))
	}
	if len(fs.patchComments) != 1 {
		t.Errorf("expected 1 status update, got %d", len(fs.patchComments))
	}
	if len(fs.removeLabels) != 1 || fs.removeLabels[0] != "aidev:scouting" {
		t.Errorf("expected removal of aidev:scouting label, got %v", fs.removeLabels)
	}
	if len(fs.addLabels) != 2 {
		t.Errorf("expected 2 label adds, got %d", len(fs.addLabels))
	}
}

func TestGitHubReporterFindsExistingStatusOnStartup(t *testing.T) {
	fs := newFakeServer(t)
	fs.listComments = []github.Comment{
		{ID: 777, Body: "irrelevant comment"},
		{ID: 888, Body: "## aidev status\n\n**old**\n\n" + statusFingerprint},
	}
	client := github.NewClientWithEndpoint("tok", fs.server.URL+"/")
	issue := &github.Issue{Owner: "o", Repo: "r", Number: 1}
	rep := NewGitHubReporter(context.Background(), client, issue, io.Discard)

	// First event should UPDATE (PATCH) the existing comment, not POST a new one.
	if err := rep.OnEvent(context.Background(), Event{State: StateScouting, Message: "restart"}); err != nil {
		t.Fatal(err)
	}

	if len(fs.postComments) != 0 {
		t.Errorf("should not have posted a new status comment, got %d posts", len(fs.postComments))
	}
	if len(fs.patchComments) != 1 || fs.patchComments[0].id != 888 {
		t.Errorf("should have patched comment 888, got %+v", fs.patchComments)
	}
}

func TestGitHubReporterPostsSketchesArtifact(t *testing.T) {
	fs := newFakeServer(t)
	client := github.NewClientWithEndpoint("tok", fs.server.URL+"/")
	issue := &github.Issue{Owner: "o", Repo: "r", Number: 1}
	rep := NewGitHubReporter(context.Background(), client, issue, io.Discard)
	ctx := context.Background()

	_ = rep.OnEvent(ctx, Event{State: StateScouting, Message: "x"})
	_ = rep.OnEvent(ctx, Event{State: StateCritiquing, Message: "y", Report: &agents.Context{ScoutReport: "brief"}})
	_ = rep.OnEvent(ctx, Event{State: StateAwaitUser, Message: "z", Report: &agents.Context{ScoutReport: "brief", CriticReport: "report"}})
	_ = rep.OnEvent(ctx, Event{State: StateArchitecting, Message: "w"})
	_ = rep.OnEvent(ctx, Event{
		State:   StateSketchesReady,
		Message: "done",
		Report: &agents.Context{
			ScoutReport:  "brief",
			CriticReport: "report",
			Sketches: []agents.Sketch{
				{Number: 1, Title: "Postgres", Markdown: "## Sketch 1: Postgres\n\nuse pg"},
				{Number: 2, Title: "Redis", Markdown: "## Sketch 2: Redis\n\nuse redis"},
			},
		},
	})

	// Count sketches artifact: it's the POST whose body contains "## 📐 Architect".
	sketchCount := 0
	for _, b := range fs.postComments {
		if strings.Contains(b, "Architect") && strings.Contains(b, "sketches") {
			sketchCount++
			if !strings.Contains(b, "Postgres") || !strings.Contains(b, "Redis") {
				t.Errorf("sketches comment missing titles: %q", b)
			}
		}
	}
	if sketchCount != 1 {
		t.Errorf("expected exactly 1 sketches artifact post, got %d", sketchCount)
	}
}

// Test helpers.

func intToString(n int64) string {
	// Avoid strconv to keep the import list compact in the test file.
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func parseTrailingInt(s string) int64 {
	// Parse the trailing integer of a path like ".../issues/comments/42".
	i := len(s)
	for i > 0 && s[i-1] >= '0' && s[i-1] <= '9' {
		i--
	}
	var n int64
	for j := i; j < len(s); j++ {
		n = n*10 + int64(s[j]-'0')
	}
	return n
}

func quote(s string) string {
	// Minimal JSON string quoting for the test harness.
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\n':
			b.WriteString("\\n")
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// extractStrings is a very small JSON parser for the shape
// `{"labels":["a","b","c"]}`. We avoid encoding/json to keep this file
// small and focused.
func extractStrings(body string) []string {
	start := strings.Index(body, "[")
	end := strings.LastIndex(body, "]")
	if start < 0 || end < 0 || end <= start {
		return nil
	}
	inner := body[start+1 : end]
	parts := strings.Split(inner, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		p = strings.Trim(p, `"`)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
