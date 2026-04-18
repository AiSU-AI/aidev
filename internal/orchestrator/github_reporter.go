package orchestrator

import (
	"context"
	"fmt"
	"io"
	"os"
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
			// Selector verdict: posted as its own comment so the
			// architectural decision is permanently auditable on
			// the issue. When the Selector wasn't run (older
			// configs, tests) or returned no pick, skip — the
			// pinned status still reflects the state.
			if agCtx.Selector != nil {
				if err := r.postSelectorVerdict(ctx, agCtx.Selector); err != nil {
					fmt.Fprintf(r.log, "aidev reporter: post selector verdict: %v\n", err)
				}
			}
		}
		// Selector swaps the label from architecting → selected when
		// it picked a sketch; falls back to "sketches-ready" when no
		// pick was made (the slash command treats this as the
		// "needs refinement" signal — see P5).
		phase := "sketches-ready"
		headline := "✅ Sketches ready — review and pick one"
		if agCtx := contextFromEvent(ev); agCtx != nil && agCtx.Selector != nil && agCtx.Selector.ChosenNumber > 0 {
			phase = "selected"
			headline = fmt.Sprintf("🧭 Selector chose Sketch %d (score %.2f) — handing off to implementation", agCtx.Selector.ChosenNumber, agCtx.Selector.Score)
		}
		return r.transition(ctx, phase, headline, ev.Message)
	case StateImplementing:
		return r.transition(ctx, "implementing", "🔨 Implementer generating patch...", ev.Message)
	case StatePatchReady:
		return r.transition(ctx, "patch-ready", "✅ Patch ready — review and apply", ev.Message)
	case StateTesting:
		return r.transition(ctx, "testing", "🧪 Running tests...", ev.Message)
	case StateTestsPassed:
		return r.transition(ctx, "tests-passed", "✅ Tests passed", ev.Message)
	case StateTestsFailed:
		return r.transition(ctx, "tests-failed", "❌ Tests failed", ev.Message)
	case StateReviewing:
		return r.transition(ctx, "reviewing", "👀 Reviewer auditing patch...", ev.Message)
	case StateReviewDone:
		return r.transition(ctx, "review-done", "📝 Review complete", ev.Message)
	case StateNeedsRefinement:
		// P5: aidev refused to proceed autonomously. Post a
		// structured "needs refinement" comment with the rationale
		// and actionable next steps so the human knows what to do.
		if agCtx := contextFromEvent(ev); agCtx != nil {
			if err := r.postNeedsRefinement(ctx, agCtx, ev.Message); err != nil {
				fmt.Fprintf(r.log, "aidev reporter: post needs-refinement: %v\n", err)
			}
		}
		return r.transition(ctx, "needs-refinement", "🤔 Needs refinement before implementing — see the latest comment for next steps", ev.Message)
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

// postSelectorVerdict posts the Selector's autonomous pick as a
// standalone audit-trail comment. This is the durable architectural
// decision record: anyone reviewing the issue six months later sees
// "aidev's Selector chose Sketch N because X" without having to dig
// through transient artifact files. The all-scores table is in a
// <details> block so the issue thread stays scannable.
func (r *GitHubReporter) postSelectorVerdict(ctx context.Context, v *agents.SelectorVerdict) error {
	if v == nil {
		return nil
	}
	var b strings.Builder
	if v.ChosenNumber == 0 {
		// No pick — the orchestrator + slash command will treat this
		// as "needs refinement". Post the rationale so the human
		// reviewer sees WHY no sketch was implementable.
		b.WriteString("## 🧭 Selector: no sketch chosen — needs refinement\n\n")
		fmt.Fprintf(&b, "**Rubric version:** %s\n\n", v.RubricVersion)
		fmt.Fprintf(&b, "**Rationale:** %s\n\n", v.Rationale)
	} else {
		fmt.Fprintf(&b, "## 🧭 Selector chose Sketch %d\n\n", v.ChosenNumber)
		fmt.Fprintf(&b, "**Score:** %.2f  •  **Rubric version:** %s  •  **Tie-breaker invoked:** %t\n\n", v.Score, v.RubricVersion, v.TieBreakerUsed)
		fmt.Fprintf(&b, "**Rationale:** %s\n\n", v.Rationale)
	}
	if len(v.Breakdowns) > 0 {
		b.WriteString("<details><summary>All sketch scores</summary>\n\n")
		b.WriteString("| # | Title | Score | Aligned | Tension | Violation | Risks | Notes |\n")
		b.WriteString("| --- | --- | ---: | ---: | ---: | ---: | ---: | --- |\n")
		for _, bd := range v.Breakdowns {
			notes := ""
			if bd.DisqualifyReason != "" {
				notes = "DISQUALIFIED: " + bd.DisqualifyReason
			} else if bd.SketchNumber == v.ChosenNumber {
				notes = "**CHOSEN**"
			}
			scoreCell := fmt.Sprintf("%.2f", bd.Score)
			if bd.DisqualifyReason != "" {
				scoreCell = "−∞"
			}
			fmt.Fprintf(&b, "| %d | %s | %s | %d | %d | %d | %d | %s |\n",
				bd.SketchNumber, bd.Title, scoreCell, bd.Aligned, bd.Tension, bd.Violation, bd.Risks, notes)
		}
		b.WriteString("\n</details>\n\n")
	}
	fmt.Fprintf(&b, "_Posted by aidev at %s_\n", time.Now().UTC().Format(time.RFC3339))
	_, err := r.client.PostComment(ctx, r.issue.Owner, r.issue.Repo, r.issue.Number, b.String())
	return err
}

// postNeedsRefinement composes and posts the structured refinement
// comment when aidev declines to proceed autonomously. Triggers
// covered today: the Selector returned ChosenNumber=0 (all sketches
// disqualified, OR best score below MinImplementableScore). Future
// triggers (Critic stuck on `unclear` after the Clarifier loop maxed
// out, Architect produced no viable sketches) wire through the same
// state and method.
//
// The comment is deliberately structured: a one-line "why it stopped",
// a bullet list of observations, a checklist of what the human can do
// to unblock, and a note that no PR was created. This format matches
// what the slash command tells the user to look for in STEP 6.
func (r *GitHubReporter) postNeedsRefinement(ctx context.Context, c *agents.Context, fallbackReason string) error {
	var b strings.Builder
	b.WriteString("## 🤔 aidev needs refinement before implementing\n\n")

	reason := fallbackReason
	if c.Selector != nil && c.Selector.Rationale != "" {
		reason = c.Selector.Rationale
	}
	if reason == "" {
		reason = "The pipeline could not find an implementable sketch."
	}
	fmt.Fprintf(&b, "**Why it stopped:** %s\n\n", reason)

	b.WriteString("**What aidev observed:**\n")
	if c.ScoutReport != "" {
		b.WriteString("- Scout produced a brief; check it for any architecture drift the issue body assumes wrong.\n")
	}
	if c.CriticReport != "" {
		b.WriteString("- Critic raised sharp questions worth addressing; see the Critic comment above.\n")
	}
	if c.Selector != nil && len(c.Selector.Disqualified) > 0 {
		fmt.Fprintf(&b, "- Selector disqualified %d of %d sketches against the rubric (see the Selector comment above for the per-sketch breakdown).\n",
			len(c.Selector.Disqualified), len(c.Selector.Breakdowns))
	}

	b.WriteString("\n**What it needs from a human:**\n")
	b.WriteString("- [ ] **Decompose into smaller issues** if the scope mixes multiple concerns\n")
	b.WriteString("- [ ] **Add acceptance criteria** if the issue body is too vague (\"what does done look like?\")\n")
	b.WriteString("- [ ] **Choose between approaches** if Architect sketches reveal an irreconcilable trade-off\n")
	b.WriteString("- [ ] **Provide examples / fixtures / screenshots** if verification needs a concrete reference\n")
	b.WriteString("- [ ] **Other** — see the Critic's sharp questions above\n\n")

	b.WriteString("**Suggested next actions:**\n")
	b.WriteString("- Edit the issue body to address the gaps above, then re-run `/aidev-run <num>`.\n")
	b.WriteString("- (Future) `/aidev-research <num>` for deeper repo + web research.\n")
	b.WriteString("- (Future) `/aidev-plan <num>` to flesh out a multi-step plan with checkpoints.\n")
	b.WriteString("- (Future) `/aidev-decompose <num>` to propose child issues with human approval before filing.\n")
	b.WriteString("- Manually file child issues if decomposition is needed.\n")
	b.WriteString("- Close the issue if it's no longer relevant.\n\n")

	b.WriteString("_The pipeline stopped before implementation. Working tree is clean. No PR was created._\n")
	b.WriteString("\n")
	fmt.Fprintf(&b, "_Posted by aidev at %s_\n", time.Now().UTC().Format(time.RFC3339))

	_, err := r.client.PostComment(ctx, r.issue.Owner, r.issue.Repo, r.issue.Number, b.String())
	return err
}

// PostForceVerdictOverride posts a loud, audit-trail comment when the
// user passed `-force-verdict` to override the Critic's recommendation.
// The override is also logged to stderr at the call site, but the GH
// comment is what survives in the issue history. Called from main.go
// when the override fires; safe to call from outside OnEvent because
// the override is a CLI-flag concern, not a state-machine transition.
func (r *GitHubReporter) PostForceVerdictOverride(ctx context.Context, originalVerdict, forcedVerdict string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	body := fmt.Sprintf(`## 🚨 Human override: Critic verdict forced

Critic recommended **%s**.
User passed `+"`-force-verdict %s`"+`. Proceeding as if the Critic had said `+"`%s`"+`.

The Critic's report above is the audit trail; this override is the user's call and responsibility. The pipeline will continue to the Architect with the forced verdict.

_Posted by aidev at %s_
`, originalVerdict, forcedVerdict, forcedVerdict, time.Now().UTC().Format(time.RFC3339))
	_, err := r.client.PostComment(ctx, r.issue.Owner, r.issue.Repo, r.issue.Number, body)
	return err
}

// PostClarifierSession reads the on-disk clarifier markdown and posts
// it as an audit-trail comment so the Critic's sharp questions and
// the answers (whether from the human or evidence-based by Claude
// Code) are durable on the issue. Called from main.go after a
// successful WriteClarifierMarkdown. Returns nil when the file is
// missing — clarifier sessions are optional in the pipeline.
func (r *GitHubReporter) PostClarifierSession(ctx context.Context, clarifierPath string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	data, err := os.ReadFile(clarifierPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read clarifier %s: %w", clarifierPath, err)
	}
	body := fmt.Sprintf(`## ❓ Clarifier session

The Critic asked sharp questions; here are the answers that aidev re-evaluated against.

<details><summary><strong>Clarifier Q&A</strong> <sub>(aidev, %s)</sub></summary>

%s

</details>
`, time.Now().UTC().Format(time.RFC3339), string(data))
	_, err = r.client.PostComment(ctx, r.issue.Owner, r.issue.Repo, r.issue.Number, body)
	return err
}

// PostCoordinatorNote posts a standalone audit-trail comment for a
// Coordinator concern at warn or concern severity. info severity stays
// in the pinned status only — promoting every info to a comment would
// flood the issue thread. Called from the orchestrator's runAdvisoryGate
// when the note's severity is at or above the threshold.
func (r *GitHubReporter) PostCoordinatorNote(ctx context.Context, gateName, severity, body string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	emoji := "⚠️"
	if severity == "concern" {
		emoji = "🚨"
	}
	wrapped := fmt.Sprintf(`## %s Coordinator %s at %s

%s

_(This is advisory; the pipeline continued. Review whether this affects downstream decisions. Posted by aidev at %s.)_
`, emoji, severity, gateName, body, time.Now().UTC().Format(time.RFC3339))
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
