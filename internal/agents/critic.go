package agents

import (
	"context"
	"fmt"
	"strings"

	"github.com/aisu-ai/aidev/internal/llm"
)

// Critic is the second agent and the distinguishing feature of aidev. It
// takes the Scout's brief plus the issue body and produces an explicit
// "should this be built?" decision backed by the loaded principles.
//
// The Critic is intentionally adversarial. Its prompt instructs it to argue
// BOTH sides with equal vigor and then commit to a recommendation; the
// orchestrator gates the pipeline on the user accepting (or overriding)
// that recommendation.
type Critic struct {
	Provider llm.Provider
}

// NewCritic builds a Critic from the router's RoleCritic mapping (typically
// the large, cloud-hosted tier).
func NewCritic(router *llm.Router) (*Critic, error) {
	p, err := router.For(llm.RoleCritic)
	if err != nil {
		return nil, err
	}
	return &Critic{Provider: p}, nil
}

// Report is the structured output of a Critic run. We keep it as plain
// Markdown rather than a tagged struct because the TUI needs to render it
// and the downstream Architect agent will quote it verbatim.
type Report struct {
	Markdown       string
	Recommendation string // "build" | "defer" | "kill" | "unclear"
}

// Run produces a decision report. It fails loudly if the shared Context is
// missing the issue or the scout report.
func (c *Critic) Run(ctx context.Context, cc *Context) (*Report, error) {
	if cc == nil || cc.Issue == nil {
		return nil, fmt.Errorf("critic: missing issue")
	}
	if cc.ScoutReport == "" {
		return nil, fmt.Errorf("critic: scout report must run first")
	}

	system := `You are the Critic for aidev, a multi-agent coding tool.

Your single responsibility is to decide whether a proposed GitHub issue
SHOULD be implemented on this repository, given the Scout's factual brief
and the engineering principles provided. You are adversarial on purpose.

You MUST:
1. Restate the proposal in one sentence (no editorial).
2. Argue FOR the proposal in 3-5 bullets. Cite the repo's stated purpose.
3. Argue AGAINST the proposal in 3-5 bullets. Cite the principles by name.
   Call out scope creep, YAGNI violations, reversibility cost, and any
   cheaper alternative that already exists in the repo.
4. Surface 2-3 sharp questions the human should answer before the team
   invests effort.
5. End with EXACTLY ONE line of the form:

       RECOMMENDATION: build
   or  RECOMMENDATION: defer
   or  RECOMMENDATION: kill
   or  RECOMMENDATION: unclear

Do NOT hedge the recommendation. If the evidence is mixed, choose "unclear"
and say what evidence would move you.`

	// Marshal the principles into a compact, quotable block.
	var principles strings.Builder
	for _, p := range cc.Principles {
		fmt.Fprintf(&principles, "- **%s** — %s\n  %s\n", p.Name, p.Summary, oneLine(p.Description))
	}

	var user strings.Builder
	fmt.Fprintf(&user, "## GitHub issue %s/%s#%d: %s\n\n", cc.Issue.Owner, cc.Issue.Repo, cc.Issue.Number, cc.Issue.Title)
	user.WriteString(cc.Issue.Body)
	user.WriteString("\n\n## Scout brief\n\n")
	user.WriteString(cc.ScoutReport)
	user.WriteString("\n\n## Engineering principles\n\n")
	user.WriteString(principles.String())

	resp, err := c.Provider.Complete(ctx, llm.Request{
		System: system,
		Messages: []llm.Message{
			{Role: "user", Content: user.String()},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("critic: %w", err)
	}

	return &Report{
		Markdown:       resp.Content,
		Recommendation: extractRecommendation(resp.Content),
	}, nil
}

// extractRecommendation returns the one-word verdict from the tail of the
// report. Unknown or missing verdicts collapse to "unclear" so the
// orchestrator always has something to render.
func extractRecommendation(md string) string {
	lines := strings.Split(md, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(strings.ToUpper(line), "RECOMMENDATION:") {
			continue
		}
		val := strings.TrimSpace(line[len("RECOMMENDATION:"):])
		val = strings.TrimSpace(strings.TrimPrefix(val, ":"))
		val = strings.ToLower(val)
		switch val {
		case "build", "defer", "kill", "unclear":
			return val
		}
	}
	return "unclear"
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	return strings.TrimSpace(s)
}
