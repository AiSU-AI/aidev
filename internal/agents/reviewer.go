package agents

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/aisu-ai/aidev/internal/llm"
)

// Reviewer is the v0.4 agent that performs a Boy Scout pass on a patch
// before the user merges it. Given a unified diff, it produces a
// structured review with three sections:
//
//	BLOCKERS  — things that would prevent merge (correctness, safety,
//	            principle violations)
//	SUGGESTIONS — non-blocking improvements inside the diff's scope
//	FOLLOW-UPS — out-of-scope improvements worth filing as separate
//	            issues (Boy Scout: "this dirty thing I noticed nearby")
//
// Reviewer does NOT auto-file the follow-ups. It writes them to
// `<repo>/.aidev/followups.md` for the user to review and either file
// manually or with a future `aidev followups --file-issues` command.
// This keeps the safety story "propose, never mutate" consistent with
// the Implementer.
type Reviewer struct {
	Provider llm.Provider
}

// NewReviewer builds a Reviewer from the router's RoleReviewer mapping
// (defaults to medium tier in the shipped config).
func NewReviewer(router *llm.Router) (*Reviewer, error) {
	p, err := router.For(llm.RoleReviewer)
	if err != nil {
		return nil, err
	}
	return &Reviewer{Provider: p}, nil
}

// Review is a complete Reviewer output.
type Review struct {
	Markdown  string         // full markdown report, ready to post to a PR
	Blockers  []ReviewNote   // parsed blockers
	Suggests  []ReviewNote   // parsed suggestions
	FollowUps []FollowUpIssue // parsed proposed follow-up issues
	Verdict   string         // "approve" | "changes_requested" | "comment"
}

// ReviewNote is a single blocker or suggestion line from the report.
type ReviewNote struct {
	Severity string // "blocker" | "suggestion"
	Text     string
}

// FollowUpIssue is a proposed out-of-scope improvement. The user can
// review these in `.aidev/followups.md` and decide which to file.
type FollowUpIssue struct {
	Title  string
	Body   string
	Labels []string
}

// Run produces a Review for the given patch and repo context. The patch
// must be a non-empty unified diff; the Implementer no longer emits a
// `# no-op` sentinel, so any non-diff content here is treated as an
// error by the downstream review prompt.
func (r *Reviewer) Run(ctx context.Context, c *Context, patch string) (*Review, error) {
	if c == nil || c.Issue == nil {
		return nil, errors.New("reviewer: missing issue")
	}
	patch = strings.TrimSpace(patch)
	if patch == "" {
		return nil, errors.New("reviewer: empty patch")
	}

	system := "You are the Reviewer for aidev, a multi-agent coding tool. A " +
		"diff has just been produced by the Implementer and the user is about " +
		"to apply it. Your job is a Boy Scout pass on that diff: identify " +
		"anything that should block merge, anything the author could still " +
		"improve inside the scope of the change, and anything nearby that " +
		"deserves a follow-up issue.\n\n" +
		"Produce EXACTLY these sections in Markdown:\n\n" +
		"# Review\n\n" +
		"## Blockers\n" +
		"Bullet list. Cite the principle violated and the specific line/file. " +
		"Be willing to find zero blockers — do not invent them.\n\n" +
		"## Suggestions\n" +
		"Bullet list. Non-blocking improvements the author could still make " +
		"INSIDE the scope of this diff. Name the file and approximate region.\n\n" +
		"## Follow-up issues\n" +
		"For each proposal use this format:\n\n" +
		"- **<short title>** — <why this is worth filing> (labels: label1, label2)\n\n" +
		"Only propose follow-ups for things the diff NEARLY touched but leaving " +
		"them in scope would be creep. Boy Scout rule: 'leave the code cleaner', " +
		"not 'rewrite half the repo'.\n\n" +
		"## Verdict\n\n" +
		"End with EXACTLY one line of the form:\n\n" +
		"    VERDICT: approve\n" +
		"or  VERDICT: changes_requested\n" +
		"or  VERDICT: comment\n\n" +
		"Do not hedge. If blockers is empty, verdict is approve. Otherwise " +
		"verdict is changes_requested."

	var principles strings.Builder
	for _, p := range c.Principles {
		fmt.Fprintf(&principles, "- **%s** — %s\n", p.Name, p.Summary)
	}

	user := fmt.Sprintf(
		"## Issue %s/%s#%d: %s\n\n%s\n\n## Chosen sketch summary\n\n(see Critic report for context)\n\n## Patch to review\n\n```diff\n%s\n```\n\n## Engineering principles\n\n%s",
		c.Issue.Owner, c.Issue.Repo, c.Issue.Number, c.Issue.Title,
		c.Issue.Body, patch, principles.String(),
	)

	resp, err := r.Provider.Complete(ctx, llm.Request{
		System: system,
		Messages: []llm.Message{
			{Role: "user", Content: user},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("reviewer: %w", err)
	}

	md := strings.TrimSpace(resp.Content)
	rev := &Review{
		Markdown:  md,
		Blockers:  parseReviewNotes(md, "Blockers", "blocker"),
		Suggests:  parseReviewNotes(md, "Suggestions", "suggestion"),
		FollowUps: parseFollowUps(md),
		Verdict:   extractVerdict(md),
	}
	return rev, nil
}

// WriteFollowUps persists the proposed follow-ups to
// `<dir>/followups.md`. dir is the artifact directory the file lands
// in directly (NO ".aidev" subdirectory is created — callers pass the
// runDir from internal/runpath). Idempotent per run — overwrites any
// existing file. Returns the path written.
func (r *Review) WriteFollowUps(dir string) (string, error) {
	if r == nil {
		return "", errors.New("reviewer: nil review")
	}
	if len(r.FollowUps) == 0 {
		return "", nil
	}
	if dir == "" {
		return "", errors.New("reviewer: empty target directory")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("reviewer: mkdir: %w", err)
	}
	path := filepath.Join(dir, "followups.md")

	var b strings.Builder
	b.WriteString("# Follow-up issues proposed by the Reviewer\n\n")
	fmt.Fprintf(&b, "_Generated by aidev on %s. Review these, then file manually or wait for `aidev followups --file-issues` in a future release._\n\n", time.Now().UTC().Format("2006-01-02"))
	for i, f := range r.FollowUps {
		fmt.Fprintf(&b, "## %d. %s\n\n", i+1, f.Title)
		if len(f.Labels) > 0 {
			fmt.Fprintf(&b, "labels: %s\n\n", strings.Join(f.Labels, ", "))
		}
		b.WriteString(f.Body)
		b.WriteString("\n\n---\n\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return "", fmt.Errorf("reviewer: write: %w", err)
	}
	return path, nil
}

// parseReviewNotes extracts bullet items from the named section of a
// review report. Section boundaries are "## <name>" headers; bullets
// start with "- " at the left margin.
func parseReviewNotes(md, section, severity string) []ReviewNote {
	body := extractSection(md, section)
	if body == "" {
		return nil
	}
	var out []ReviewNote
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "- ") {
			continue
		}
		text := strings.TrimSpace(strings.TrimPrefix(line, "- "))
		if text == "" {
			continue
		}
		out = append(out, ReviewNote{Severity: severity, Text: text})
	}
	return out
}

// followUpLineRe matches the structured follow-up bullet produced by
// the system prompt: `- **Title** — description (labels: a, b)`
var followUpLineRe = regexp.MustCompile(`^- \*\*(.+?)\*\*\s*[—-]\s*(.+?)(?:\s*\(labels:\s*(.+?)\))?$`)

// parseFollowUps extracts the structured follow-up list from the
// "Follow-up issues" section. Lines that don't match the expected shape
// are skipped — the model occasionally adds a summary sentence that
// shouldn't become an issue.
func parseFollowUps(md string) []FollowUpIssue {
	body := extractSection(md, "Follow-up issues")
	if body == "" {
		return nil
	}
	var out []FollowUpIssue
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "- ") {
			continue
		}
		m := followUpLineRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		var labels []string
		if m[3] != "" {
			for _, l := range strings.Split(m[3], ",") {
				l = strings.TrimSpace(l)
				if l != "" {
					labels = append(labels, l)
				}
			}
		}
		out = append(out, FollowUpIssue{
			Title:  strings.TrimSpace(m[1]),
			Body:   strings.TrimSpace(m[2]),
			Labels: labels,
		})
	}
	return out
}

// extractSection returns the body of the section whose heading is
// "## <name>". The body runs until the next "## " heading or the end of
// the document.
func extractSection(md, name string) string {
	lines := strings.Split(md, "\n")
	var start, end int
	start, end = -1, len(lines)
	needle := "## " + name
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if start < 0 {
			if trimmed == needle {
				start = i + 1
			}
			continue
		}
		if strings.HasPrefix(trimmed, "## ") {
			end = i
			break
		}
	}
	if start < 0 || start >= end {
		return ""
	}
	return strings.TrimSpace(strings.Join(lines[start:end], "\n"))
}

// extractVerdict finds the trailing "VERDICT: <word>" line and returns
// the canonical lowercase form. Unknown or missing verdicts collapse to
// "comment" so the Reporter always has something meaningful to show.
func extractVerdict(md string) string {
	lines := strings.Split(md, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(strings.ToUpper(line), "VERDICT:") {
			continue
		}
		val := strings.TrimSpace(line[len("VERDICT:"):])
		val = strings.ToLower(strings.TrimSpace(val))
		switch val {
		case "approve", "changes_requested", "comment":
			return val
		}
	}
	return "comment"
}
