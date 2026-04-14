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
and say what evidence would move you.

If a "Clarifier session" section is present in the input below, it
contains the human's direct answers to sharp questions YOU raised in a
prior pass. Treat those answers as AUTHORITATIVE GROUND TRUTH:

  - The user has decided. Their answers are not a starting point for
    further interrogation.
  - You may NOT re-ask any question that has already been answered in
    the Clarifier session, even in a slightly rephrased form. If you
    find yourself wanting to demand more evidence for an answered
    question, the answer itself IS the evidence — that is the entire
    point of the Clarifier loop.
  - You may still raise NEW questions that the existing answers
    surfaced — but only if those questions could not have been
    foreseen from the original sharp questions you raised.
  - If the Clarifier section materially resolves the ambiguity that
    drove a prior 'unclear' or 'defer' verdict, your new verdict
    SHOULD be 'build' (or, if the answers reveal a fatal flaw, 'kill').
    Re-emitting 'unclear' or 'defer' after the user has answered is a
    failure mode: it means the loop made no progress and the user is
    stuck. Avoid it unless the answers literally created NEW
    ambiguity.`

	// Marshal the principles into a compact, quotable block.
	var principles strings.Builder
	for _, p := range cc.Principles {
		fmt.Fprintf(&principles, "- **%s** — %s\n  %s\n", p.Name, p.Summary, oneLine(p.Description))
	}

	// Resolve the Clarifier content for this run. Two sources can supply
	// it and they MUST converge on the same authoritative section in
	// the prompt — otherwise the slash-command flow (which writes
	// .aidev/clarifier.md to disk) would silently bypass the
	// "treat as authoritative" plumbing the in-process TTY interview
	// goes through:
	//
	//  1. cc.ClarifierNotes — set by the in-process headless TTY
	//     interview right before calling Recritique. Highest priority
	//     because it reflects the answers collected in THIS run.
	//
	//  2. cc.Snapshot.ClarifierContent — the persisted .aidev/clarifier.md
	//     file on disk, populated by the repo Scout snapshot. This is
	//     how Claude Code's slash-command interview hands answers to
	//     aidev: it writes the file, then re-invokes us. Used only when
	//     ClarifierNotes is empty so the in-memory copy always wins.
	clarifier := strings.TrimSpace(cc.ClarifierNotes)
	if clarifier == "" && cc.Snapshot != nil {
		clarifier = strings.TrimSpace(cc.Snapshot.ClarifierContent)
	}

	var user strings.Builder
	fmt.Fprintf(&user, "## GitHub issue %s/%s#%d: %s\n\n", cc.Issue.Owner, cc.Issue.Repo, cc.Issue.Number, cc.Issue.Title)
	user.WriteString(cc.Issue.Body)
	user.WriteString("\n\n## Scout brief\n\n")
	user.WriteString(cc.ScoutReport)
	user.WriteString("\n\n## Engineering principles\n\n")
	user.WriteString(principles.String())
	if clarifier != "" {
		user.WriteString("\n\n## Clarifier session (human answers to your previous sharp questions — AUTHORITATIVE GROUND TRUTH)\n\n")
		user.WriteString(clarifier)
	}

	resp, err := c.Provider.Complete(ctx, llm.Request{
		System: system,
		Messages: []llm.Message{
			{Role: "user", Content: user.String()},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("critic: %w", err)
	}

	rpt := &Report{
		Markdown:       resp.Content,
		Recommendation: extractRecommendation(resp.Content),
	}
	// Stash the Markdown on the shared Context so downstream agents
	// (Architect) can quote it without reaching into the orchestrator.
	cc.CriticReport = rpt.Markdown
	return rpt, nil
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
