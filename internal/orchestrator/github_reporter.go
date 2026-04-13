package orchestrator

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/aisu-ai/aidev/internal/agents"
	"github.com/aisu-ai/aidev/internal/github"
)

// GitHubReporter posts progress and artifacts to the issue that seeded
// the current run. It maintains one pinned status comment (updated in
// place for every transition) and one-shot artifact comments for
// Scout/Critic/Architect output.
//
// The reporter also swaps a single `aidev:<phase>` label on the issue so
// the repo's Issues page gets a kanban-style progress indicator for free.
type GitHubReporter struct {
	client *github.Client
	issue  *github.Issue

	// log is where non-fatal errors are written. The orchestrator never
	// dies because of a GitHubReporter failure — if the network is down
	// or the token is scoped wrong, we log and continue. Defaults to
	// io.Discard when unset.
	log io.Writer

	mu              sync.Mutex
	statusCommentID int64  // 0 until the pinned status comment is created
	currentLabel    string // last aidev:<phase> label we added, for removal on next transition
	closed          bool
}

// fingerprint string lives inside the HTML comment at the bottom of the
// pinned status comment, so cross-run reuse can locate it by scanning
// issue comments. Keep the string literal stable — it's a public contract
// with existing comments on GitHub.
const statusFingerprint = "<!-- aidev:status -->"

// NewGitHubReporter constructs a reporter for the given issue. It
// immediately scans the issue's existing comments looking for a prior
// status comment (by fingerprint) so re-runs update in place instead of
// posting a duplicate pinned comment.
//
// issue must be non-nil. log may be nil (a no-op discard logger is used).
func NewGitHubReporter(ctx context.Context, client *github.Client, issue *github.Issue, log io.Writer) *GitHubReporter {
	if log == nil {
		log = io.Discard
	}
	r := &GitHubReporter{client: client, issue: issue, log: log}

	// Best-effort discovery of an existing status comment from a prior
	// run. A failure here is silently ignored — we'll just create a new
	// one on the first OnEvent.
	comments, err := client.ListComments(ctx, issue.Owner, issue.Repo, issue.Number)
	if err != nil {
		fmt.Fprintf(log, "aidev reporter: cannot list existing comments: %v\n", err)
		return r
	}
	for _, c := range comments {
		if strings.Contains(c.Body, statusFingerprint) {
			r.statusCommentID = c.ID
			break
		}
	}
	return r
}

// OnEvent routes a state transition to the right GitHub side effects. It
// returns nil on success and the error from GitHub on failure — the
// orchestrator logs errors but does not abort on them.
func (r *GitHubReporter) OnEvent(ctx context.Context, ev Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}

	switch ev.State {
	case StateScouting:
		return r.transition(ctx, "scouting", "🔍 Scout running...", ev.Message)
	case StateCritiquing:
		// Scout just finished; post its brief as a one-shot.
		if agCtx := contextFromEvent(ev); agCtx != nil && agCtx.ScoutReport != "" {
			if err := r.postArtifact(ctx, "Scout brief", agCtx.ScoutReport); err != nil {
				fmt.Fprintf(r.log, "aidev reporter: post scout artifact: %v\n", err)
			}
		}
		return r.transition(ctx, "critiquing", "🔎 Critic running...", ev.Message)
	case StateAwaitUser:
		if agCtx := contextFromEvent(ev); agCtx != nil && agCtx.CriticReport != "" {
			if err := r.postArtifact(ctx, "Critic report", agCtx.CriticReport); err != nil {
				fmt.Fprintf(r.log, "aidev reporter: post critic artifact: %v\n", err)
			}
		}
		return r.transition(ctx, "awaiting-decision", "⏸ Awaiting human decision", ev.Message)
	case StateArchitecting:
		return r.transition(ctx, "architecting", "📐 Architect running...", ev.Message)
	case StateSketchesReady:
		if agCtx := contextFromEvent(ev); agCtx != nil && len(agCtx.Sketches) > 0 {
			if err := r.postSketches(ctx, agCtx.Sketches); err != nil {
				fmt.Fprintf(r.log, "aidev reporter: post sketches: %v\n", err)
			}
		}
		return r.transition(ctx, "sketches-ready", "✅ Sketches ready — review and pick one", ev.Message)
	case StateKilled:
		return r.transition(ctx, "killed", "🛑 Killed", ev.Message)
	case StateError:
		msg := ev.Message
		if ev.Err != nil {
			msg = "❌ Error: " + ev.Err.Error()
		}
		return r.transition(ctx, "errored", msg, "")
	}
	return nil
}

// Close stops further event processing. It does not delete the status
// comment — the trail is meant to survive the run.
func (r *GitHubReporter) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	return nil
}

// transition is the combined "update status + swap label" operation every
// state change runs. phase is the short word used in the label
// (`aidev:<phase>`); headline is the visible summary; detail is an
// optional free-form line appended after the headline.
func (r *GitHubReporter) transition(ctx context.Context, phase, headline, detail string) error {
	body := r.renderStatus(headline, detail)

	// Upsert the pinned status comment.
	if r.statusCommentID == 0 {
		posted, err := r.client.PostComment(ctx, r.issue.Owner, r.issue.Repo, r.issue.Number, body)
		if err != nil {
			return fmt.Errorf("post status: %w", err)
		}
		r.statusCommentID = posted.ID
	} else {
		if err := r.client.UpdateComment(ctx, r.issue.Owner, r.issue.Repo, r.statusCommentID, body); err != nil {
			return fmt.Errorf("update status: %w", err)
		}
	}

	// Swap phase label. Remove the previous one first so there's always
	// exactly one aidev:<phase> label on the issue at any time.
	wantLabel := "aidev:" + phase
	if r.currentLabel != "" && r.currentLabel != wantLabel {
		if err := r.client.RemoveLabel(ctx, r.issue.Owner, r.issue.Repo, r.issue.Number, r.currentLabel); err != nil {
			fmt.Fprintf(r.log, "aidev reporter: remove old label %q: %v\n", r.currentLabel, err)
		}
	}
	if r.currentLabel != wantLabel {
		if err := r.client.AddLabels(ctx, r.issue.Owner, r.issue.Repo, r.issue.Number, wantLabel); err != nil {
			fmt.Fprintf(r.log, "aidev reporter: add label %q: %v\n", wantLabel, err)
		}
		r.currentLabel = wantLabel
	}
	return nil
}

// postArtifact posts a one-shot comment containing a single agent's
// output, wrapped in a collapsed <details> block so the issue doesn't
// turn into a wall of text.
func (r *GitHubReporter) postArtifact(ctx context.Context, title, body string) error {
	wrapped := fmt.Sprintf(`<details><summary><strong>%s</strong> <sub>(aidev, %s)</sub></summary>

%s

</details>
`, title, time.Now().UTC().Format(time.RFC3339), body)
	_, err := r.client.PostComment(ctx, r.issue.Owner, r.issue.Repo, r.issue.Number, wrapped)
	return err
}

// postSketches posts the Architect's sketches as a single comment with
// each sketch inside its own <details> block. Users can expand the
// interesting ones and skim the rest.
func (r *GitHubReporter) postSketches(ctx context.Context, sketches []agents.Sketch) error {
	var b strings.Builder
	fmt.Fprintf(&b, "## 📐 Architect: %d sketches\n\n", len(sketches))
	for _, s := range sketches {
		fmt.Fprintf(&b, "<details><summary><strong>Sketch %d: %s</strong></summary>\n\n", s.Number, s.Title)
		b.WriteString(s.Markdown)
		b.WriteString("\n\n</details>\n\n")
	}
	fmt.Fprintf(&b, "_Posted by aidev at %s_\n", time.Now().UTC().Format(time.RFC3339))
	_, err := r.client.PostComment(ctx, r.issue.Owner, r.issue.Repo, r.issue.Number, b.String())
	return err
}

// renderStatus builds the body of the pinned status comment. The
// fingerprint at the end makes the comment findable on startup.
func (r *GitHubReporter) renderStatus(headline, detail string) string {
	var b strings.Builder
	b.WriteString("## aidev status\n\n")
	b.WriteString("**")
	b.WriteString(headline)
	b.WriteString("**\n")
	if detail != "" {
		b.WriteString("\n")
		b.WriteString(detail)
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "\n_Updated %s_\n\n", time.Now().UTC().Format(time.RFC3339))
	b.WriteString(statusFingerprint)
	return b.String()
}

// contextFromEvent pulls the agents.Context pointer out of an Event. It's
// a thin helper because the field is named Report for historical reasons
// and we don't want to rename it on the public struct.
func contextFromEvent(ev Event) *agents.Context {
	return ev.Report
}
