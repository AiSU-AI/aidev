// Coordinator is aidev's Gate-1 quality safety net. It sits between
// the Implementer and the patch write-to-disk step, reviews the diff
// the Implementer just produced, and decides whether to approve it or
// send it back with specific actionable feedback.
//
// The Coordinator is especially valuable when the Implementer runs on
// a local model (see `models.local.yaml`): local coder models are
// strong at line-level transformation but weak at noticing when they
// fabricated a value, used invalid syntax for the target file format,
// or left a half-finished consolidation. A cloud-backed Coordinator
// catches those failure modes and surfaces them as concrete feedback
// the Implementer can address on the next attempt.
//
// Gate 1 is deliberately the only gate the Coordinator runs at for
// now. Pre-Architect skip and post-Reviewer follow-up triage are
// possible later additions but the diff-review gate delivers the
// most value per LLM call and is the easiest to scope well.
package agents

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aisu-ai/aidev/internal/llm"
)

// Coordinator is the cross-agent monitor (v0.5). It runs at five
// gates across an aidev pipeline run:
//
//	Gate 0  — post-Scout       advisory, observes the Scout brief
//	Gate 1  — post-Critic      advisory, observes the Critic verdict
//	Gate 2  — post-Architect   advisory, observes the sketch set
//	Gate 4  — post-Implementer interventional, blocks the patch on
//	                            CONCERNS and triggers Implementer retry
//	Gate 5  — post-Reviewer    advisory, observes the Reviewer's verdict
//
// (Gate numbers leave room for future Gate 3 = pre-Architect skip
// decision; not in scope for v0.5.)
//
// Gates 0/1/2/5 are advisory: their output goes into a running
// notes buffer that downstream gates can read AND the headless
// report surfaces to the user. They never block the run.
//
// Gate 4 is the only interventional gate. Its semantics are
// unchanged from v0.4: CONCERNS triggers a bounded retry loop in
// orchestrator.Implement, with the bullets fed back to the
// Implementer as authoritative feedback.
//
// The Coordinator's own provider is invoked once per gate, so a
// full pipeline run with every gate firing is 5 LLM calls on the
// oversight tier (typically claude-cli). Cost is bounded and
// predictable: same 5 calls regardless of how complex the
// underlying issue is.
type Coordinator struct {
	Provider llm.Provider

	// Notes is the running observation log written by gates 0,
	// 1, 2, and 5. Gate 4 reads this list as additional context
	// when it reviews the Implementer's diff (so e.g. a Scout
	// note flagging "the i18n directory has 9 locales but the
	// brief only enumerates 3" can inform the diff review's
	// scope check). The orchestrator surfaces the notes in the
	// headless report so the user sees what the monitor caught.
	Notes []GateNote
}

// GateNote is one observation from an advisory gate. Severity is
// one of "info", "warn", "concern" — "concern" is louder and
// shows up in the headless report's TL;DR; the others are
// background context.
type GateNote struct {
	Gate     string // "post-scout" | "post-critic" | "post-architect" | "post-reviewer"
	Severity string // "info" | "warn" | "concern"
	Body     string // markdown, multi-line OK
}

// NewCoordinator builds a Coordinator from the router's
// RoleCoordinator mapping. Routing falls back to RoleCritic's tier
// when no coordinator routing is configured (see router.For) so
// users on older models.yaml files don't break.
func NewCoordinator(router *llm.Router) (*Coordinator, error) {
	p, err := router.For(llm.RoleCoordinator)
	if err != nil {
		return nil, err
	}
	return &Coordinator{Provider: p}, nil
}

// CoordinatorReview is the structured output of a single review
// pass. Approved==true means proceed; Approved==false means send the
// diff back to the Implementer with Feedback as the concrete list of
// issues to address.
type CoordinatorReview struct {
	// Approved is true when the diff is ready to hit disk.
	Approved bool

	// Feedback is the markdown-bulleted list of concerns when
	// Approved is false. Empty when Approved is true. Format:
	// "- <concrete issue with file:line context and what to do>".
	// The list is the exact text the Implementer sees as its
	// CoordinatorFeedback on the retry.
	Feedback string

	// Rationale is a one-line summary of WHY the Coordinator
	// reached its verdict. On approval it reads like a commit
	// message; on rejection it reads like a one-sentence code
	// review summary. Used in headless output and reporter
	// artifacts so the user can see the Coordinator's reasoning
	// without reading every bullet.
	Rationale string

	// Raw is the verbatim markdown response from the model, kept
	// for debugging and for the headless output's advisory block
	// when the retry cap is reached but the diff is still useful.
	Raw string
}

// Review asks the Coordinator to evaluate the given patch against
// the issue, sketch, scout brief, and principles in cc. Returns a
// CoordinatorReview or an error from the underlying Provider.
//
// The caller (orchestrator.Implement) is responsible for the retry
// loop: on Approved==true proceed to WriteTo; on Approved==false
// stash Feedback onto cc.CoordinatorFeedback and call
// implementer.Run again, bounded by a retry cap.
func (co *Coordinator) Review(ctx context.Context, cc *Context, chosen *Sketch, patch *Patch) (*CoordinatorReview, error) {
	if co == nil || co.Provider == nil {
		return nil, errors.New("coordinator: nil provider")
	}
	if cc == nil || cc.Issue == nil {
		return nil, errors.New("coordinator: missing issue")
	}
	if chosen == nil {
		return nil, errors.New("coordinator: missing chosen sketch")
	}
	if patch == nil || strings.TrimSpace(patch.Diff) == "" {
		return nil, errors.New("coordinator: empty patch")
	}

	system := "You are the Coordinator for aidev, a multi-agent coding tool.\n" +
		"\n" +
		"You are a senior engineer reviewing a unified git diff that the\n" +
		"Implementer just produced. The diff has NOT been written to disk\n" +
		"yet — your review decides whether it ships as-is or goes back to\n" +
		"the Implementer with feedback. The developer trusts you as the\n" +
		"last line of defense against the Implementer's most common\n" +
		"failure modes. Be strict but constructive.\n" +
		"\n" +
		"REVIEW RUBRIC — look for:\n" +
		"\n" +
		"1. FABRICATION. Any content-bearing literal (translation\n" +
		"   string, brand name, URL, API identifier, user-visible copy)\n" +
		"   that the Implementer invented rather than copied from a real\n" +
		"   source in the repo. If a diff adds 'Kontakt Vertrieb' as a\n" +
		"   German translation and you can't see the Implementer was\n" +
		"   shown the existing German file with that exact value, flag\n" +
		"   it — the Implementer should have used NEED_FILES.\n" +
		"\n" +
		"2. INVALID SYNTAX FOR FILE FORMAT. JSON hunks containing // or\n" +
		"   /* */ comments (JSON has no comments). YAML hunks with tab\n" +
		"   indentation (YAML requires spaces). TOML hunks with trailing\n" +
		"   commas. CSV hunks with unescaped commas in values. Check the\n" +
		"   file extension against the hunk content.\n" +
		"\n" +
		"3. AUXILIARY TODO/CHECKLIST FILES. AIDEV_TODO_*.md, NOTES.md,\n" +
		"   CHECKLIST.md, HUMAN_FOLLOWUP.md, or any markdown file that\n" +
		"   reads like 'here's what the human still needs to do'. These\n" +
		"   are NEVER acceptable. The diff IS the work. If the Implementer\n" +
		"   thought it needed a companion file to track unfinished work,\n" +
		"   the work is unfinished and the diff should be rejected with\n" +
		"   feedback to finish it.\n" +
		"\n" +
		"4. PARTIAL SCOPE. A diff that touches 8 of 9 locales, 3 of 5\n" +
		"   call sites, or half a migration. Consolidation tasks must be\n" +
		"   complete within their scope. Half-done is a regression, not\n" +
		"   a starting point.\n" +
		"\n" +
		"5. DANGLING REFERENCES. The diff deletes a key/function/import\n" +
		"   but leaves one or more call sites using the deleted name.\n" +
		"   The diff won't compile or will fail at runtime.\n" +
		"\n" +
		"6. MISSING TESTS. A diff that adds new behaviour without tests\n" +
		"   in the same diff — unless the change is purely configuration,\n" +
		"   documentation, or a truly trivial edit (a typo fix, a\n" +
		"   rename). Tests are not optional follow-ups.\n" +
		"\n" +
		"7. SCOPE CREEP. The diff reformats unrelated code, 'fixes' a\n" +
		"   nearby bug, or bundles a refactor the sketch didn't ask for.\n" +
		"   Stay inside the sketch's boundary.\n" +
		"\n" +
		"NON-ISSUES — do NOT reject for:\n" +
		"\n" +
		"  - Stylistic preferences the sketch didn't mandate\n" +
		"  - Suboptimal (but correct) algorithmic choices\n" +
		"  - Variable naming that isn't actively wrong\n" +
		"  - Things that are already tracked in .aidev/followups.md\n" +
		"\n" +
		"OUTPUT FORMAT — the first line of your response MUST be EXACTLY\n" +
		"ONE of:\n" +
		"\n" +
		"   APPROVED: <one-sentence rationale>\n" +
		"\n" +
		"   CONCERNS: <one-sentence summary>\n" +
		"\n" +
		"If you emit CONCERNS:, follow with a blank line and a bullet\n" +
		"list — one bullet per concrete issue, each with a file path\n" +
		"and what the Implementer should change. Example:\n" +
		"\n" +
		"   CONCERNS: German and French locale values are fabricated placeholders.\n" +
		"\n" +
		"   - locales/de/common.json: the value \"Kontakt Vertrieb\" for\n" +
		"     cta.contactSales is fabricated. The Implementer needs to\n" +
		"     NEED_FILES locales/de/common.json and copy the existing\n" +
		"     hero.sales value verbatim.\n" +
		"   - locales/fr/common.json: same issue — request and copy\n" +
		"     the existing French value.\n" +
		"\n" +
		"Be specific. 'The diff has problems' is not actionable\n" +
		"feedback. 'locales/de/common.json line 14 uses a made-up\n" +
		"German string' is. Every bullet should tell the Implementer\n" +
		"exactly what to change and why.\n" +
		"\n" +
		"Approve eagerly when the diff is good. You are not looking for\n" +
		"perfection — you are looking for failure modes. When you are\n" +
		"unsure, prefer approval with a one-line advisory rationale\n" +
		"over blocking — the Reviewer agent will do a second pass after\n" +
		"the patch is applied."

	var user strings.Builder
	fmt.Fprintf(&user, "## Issue %s/%s#%d: %s\n\n", cc.Issue.Owner, cc.Issue.Repo, cc.Issue.Number, cc.Issue.Title)
	user.WriteString(cc.Issue.Body)
	user.WriteString("\n\n## Scout brief\n\n")
	user.WriteString(cc.ScoutReport)
	if cc.CriticReport != "" {
		user.WriteString("\n\n## Critic report\n\n")
		user.WriteString(cc.CriticReport)
	}
	fmt.Fprintf(&user, "\n\n## Chosen sketch %d: %s\n\n", chosen.Number, chosen.Title)
	user.WriteString(chosen.Markdown)

	if len(cc.Principles) > 0 {
		user.WriteString("\n\n## Engineering principles\n\n")
		for _, p := range cc.Principles {
			fmt.Fprintf(&user, "- **%s** — %s\n", p.Name, p.Summary)
		}
	}

	user.WriteString("\n\n## Implementer diff under review\n\n")
	user.WriteString("```diff\n")
	user.WriteString(patch.Diff)
	user.WriteString("\n```\n")

	user.WriteString("\n\nApply the rubric above to this diff and emit APPROVED or CONCERNS per the output format.")

	resp, err := co.Provider.Complete(ctx, llm.Request{
		System: system,
		Messages: []llm.Message{
			{Role: "user", Content: user.String()},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("coordinator: %w", err)
	}
	return parseCoordinatorReview(resp.Content)
}

// parseCoordinatorReview extracts an APPROVED or CONCERNS verdict
// from a raw Coordinator response. Tolerates a stray leading code
// fence (some models wrap their whole response) and extra blank
// lines before the verdict. Exported indirectly via tests so the
// grammar is explicit.
func parseCoordinatorReview(raw string) (*CoordinatorReview, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("coordinator: empty response")
	}
	raw = stripCodeFence(raw)
	raw = strings.TrimSpace(raw)

	lines := strings.SplitN(raw, "\n", 2)
	first := strings.TrimSpace(lines[0])

	switch {
	case strings.HasPrefix(first, "APPROVED:"):
		rationale := strings.TrimSpace(strings.TrimPrefix(first, "APPROVED:"))
		return &CoordinatorReview{
			Approved:  true,
			Rationale: rationale,
			Raw:       raw,
		}, nil

	case strings.HasPrefix(first, "CONCERNS:"):
		rationale := strings.TrimSpace(strings.TrimPrefix(first, "CONCERNS:"))
		// The body (everything after the first line) is the
		// feedback bullet list that gets threaded back into the
		// Implementer's prompt on retry. Trim so a blank line
		// after CONCERNS: doesn't become leading whitespace in
		// the feedback.
		var feedback string
		if len(lines) > 1 {
			feedback = strings.TrimSpace(lines[1])
		}
		if feedback == "" {
			return nil, fmt.Errorf("coordinator: CONCERNS verdict with no feedback body — model should have emitted at least one bullet")
		}
		return &CoordinatorReview{
			Approved:  false,
			Feedback:  feedback,
			Rationale: rationale,
			Raw:       raw,
		}, nil

	default:
		return nil, fmt.Errorf("coordinator: expected 'APPROVED:' or 'CONCERNS:' at start of response, got %q", firstLine(raw))
	}
}
