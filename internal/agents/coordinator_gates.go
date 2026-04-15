// Coordinator advisory gates (v0.5).
//
// Five-gate monitor expansion that lets the Coordinator observe
// EVERY agent transition, not just the Implementer's diff:
//
//	Gate 0  ObserveScoutBrief         post-Scout
//	Gate 1  ObserveCriticReport       post-Critic
//	Gate 2  ObserveArchitectSketches  post-Architect
//	Gate 4  Review                    post-Implementer (in coordinator.go)
//	Gate 5  ObserveReviewerVerdict    post-Reviewer
//
// All four advisory gates share the same shape:
//
//   - One LLM call per gate.
//   - Output is parsed into one or more GateNote records via the
//     same APPROVED:/CONCERNS: grammar Gate 4 uses, but here the
//     "verdict" is severity-only (info/warn/concern) and never
//     blocks the pipeline.
//   - Notes are appended to co.Notes so downstream gates (and
//     the headless report) can read them.
//
// Failure modes — every gate is wrapped in defensive error
// handling because advisory gates must NEVER block the pipeline:
//
//   - Provider error (rate limit, network drop): logged, returns
//     nil notes, pipeline proceeds. The Coordinator is a safety
//     net, not a hard gate, except at Gate 4.
//   - Empty response: same — log + skip.
//   - Malformed verdict: log + skip.
//
// The orchestrator wires these in via observe-and-continue calls
// after each agent transition. See orchestrator.runGate for the
// wiring pattern.
package agents

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aisu-ai/aidev/internal/llm"
)

// observeBaseSystemPrompt is the shared instruction for all four
// advisory gates. Per-gate methods append a gate-specific
// "what to look for" section after this preamble.
const observeBaseSystemPrompt = `You are the Coordinator for aidev, a multi-agent coding tool.

Your job at this gate is to OBSERVE the output of one upstream
agent and report any concerns BEFORE the next agent in the
pipeline runs. You are not the final reviewer (that's a different
gate); you are the monitor watching for cross-agent failure
modes — things like the Critic contradicting the Clarifier, the
Architect ignoring a sharp question the Critic raised, the
Reviewer missing a class of issue you noticed earlier in the run.

You do NOT block the pipeline at this gate. Your output becomes
notes attached to the run that downstream gates and the user
both see. A clean run produces zero or one short notes per gate
("looks fine"); a problematic run produces concrete observations
the user can act on.

OUTPUT FORMAT — first line of your response MUST be EXACTLY ONE of:

   OK: <one-sentence summary>
       Use this when the upstream agent produced sound output
       and there's nothing the rest of the pipeline needs to
       know. Optional one-sentence summary; can be empty.

   NOTE: <severity> | <one-sentence summary>
       Use this when you have something to flag but it doesn't
       require pipeline intervention. severity is one of:
         info    — observation worth recording, low priority
         warn    — likely problem, downstream gates should
                   factor this in
         concern — high-priority issue that should appear in
                   the headless report's TL;DR

If you emit NOTE:, follow with a blank line and a markdown
bullet list of the specific observations. Be concrete: cite
file paths, line numbers, agent output verbatim where helpful.
Vague notes ("the brief seems thin") are worse than no notes.

Do NOT use this gate to second-guess the upstream agent's core
verdict. The Critic decides build/defer/kill; the Clarifier
decides what questions to ask; the Architect decides how many
sketches to produce. Your job is to flag inconsistencies and
gaps, not to relitigate decisions.`

// runObserveGate is the shared LLM call + parse logic for all
// four advisory gates. Returns the parsed notes and an error,
// but the error is intentionally "soft": callers in the
// orchestrator log it and continue rather than failing the run.
func (co *Coordinator) runObserveGate(ctx context.Context, gate string, gateSpecificContext string) ([]GateNote, error) {
	if co == nil || co.Provider == nil {
		return nil, errors.New("coordinator: nil provider")
	}
	resp, err := co.Provider.Complete(ctx, llm.Request{
		System: observeBaseSystemPrompt,
		Messages: []llm.Message{
			{Role: "user", Content: gateSpecificContext},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("coordinator %s gate: %w", gate, err)
	}
	notes, err := parseObserveResponse(gate, resp.Content)
	if err != nil {
		return nil, fmt.Errorf("coordinator %s gate parse: %w", gate, err)
	}
	co.Notes = append(co.Notes, notes...)
	return notes, nil
}

// parseObserveResponse parses the OK:/NOTE: grammar. Returns an
// empty slice for OK responses (no notes to record) and a slice
// with one GateNote for NOTE responses.
func parseObserveResponse(gate, raw string) ([]GateNote, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("empty response")
	}
	raw = stripCodeFence(raw)
	raw = strings.TrimSpace(raw)

	lines := strings.SplitN(raw, "\n", 2)
	first := strings.TrimSpace(lines[0])

	switch {
	case strings.HasPrefix(first, "OK:") || first == "OK":
		// All clear — no notes, but record an info-level
		// breadcrumb in the running list so the headless
		// report can show "post-scout: OK".
		summary := strings.TrimSpace(strings.TrimPrefix(first, "OK:"))
		if summary == "" {
			summary = "(no concerns)"
		}
		return []GateNote{{Gate: gate, Severity: "info", Body: summary}}, nil

	case strings.HasPrefix(first, "NOTE:"):
		// Parse "<severity> | <summary>" from the rest of
		// the first line.
		header := strings.TrimSpace(strings.TrimPrefix(first, "NOTE:"))
		parts := strings.SplitN(header, "|", 2)
		severity := "info"
		summary := header
		if len(parts) == 2 {
			severity = strings.ToLower(strings.TrimSpace(parts[0]))
			summary = strings.TrimSpace(parts[1])
		}
		switch severity {
		case "info", "warn", "concern":
			// ok
		default:
			severity = "info"
		}
		var body string
		if len(lines) > 1 {
			body = strings.TrimSpace(lines[1])
		}
		full := summary
		if body != "" {
			full = summary + "\n\n" + body
		}
		return []GateNote{{Gate: gate, Severity: severity, Body: full}}, nil

	default:
		return nil, fmt.Errorf("expected 'OK:' or 'NOTE:' first line, got %q", firstLine(raw))
	}
}

// ObserveScoutBrief runs Gate 0. Reads the Scout's repo brief and
// flags anything that looks under-specified for the issue at
// hand. Typical concerns: brief misses a directory the issue
// references, lists no relevant call sites, contradicts the
// repo's own README.
func (co *Coordinator) ObserveScoutBrief(ctx context.Context, cc *Context) ([]GateNote, error) {
	if cc == nil || cc.Issue == nil {
		return nil, errors.New("ObserveScoutBrief: missing issue")
	}
	if cc.ScoutReport == "" {
		return nil, errors.New("ObserveScoutBrief: missing scout report")
	}
	var b strings.Builder
	b.WriteString("# Gate: post-Scout\n\n")
	fmt.Fprintf(&b, "## Issue %s/%s#%d: %s\n\n", cc.Issue.Owner, cc.Issue.Repo, cc.Issue.Number, cc.Issue.Title)
	b.WriteString(cc.Issue.Body)
	b.WriteString("\n\n## Scout brief\n\n")
	b.WriteString(cc.ScoutReport)
	b.WriteString("\n\n## What to look for at this gate\n\n")
	b.WriteString("The Scout has just produced a factual brief of the repository. Before the Critic runs, observe whether the brief covers everything the issue actually touches:\n\n")
	b.WriteString("- Does the brief mention the directories/files the issue references?\n")
	b.WriteString("- Does it mention the project's testing/CI conventions if the issue might require tests?\n")
	b.WriteString("- Does it identify the right tech stack for the change being requested?\n")
	b.WriteString("- Are there obvious gaps (e.g. issue mentions i18n but brief never mentions a locale directory)?\n\n")
	b.WriteString("Emit OK: if the brief is fit for purpose, or NOTE: with a severity if you see specific gaps the Critic and Architect should be aware of.")
	return co.runObserveGate(ctx, "post-scout", b.String())
}

// ObserveCriticReport runs Gate 1. Reads the Critic's verdict
// and reasoning and flags inconsistencies — the verdict not
// matching the body, sharp questions that contradict the
// Clarifier (if present), or argument-against bullets that
// reference points the issue body explicitly already addressed.
func (co *Coordinator) ObserveCriticReport(ctx context.Context, cc *Context) ([]GateNote, error) {
	if cc == nil || cc.Issue == nil {
		return nil, errors.New("ObserveCriticReport: missing issue")
	}
	if cc.CriticReport == "" {
		return nil, errors.New("ObserveCriticReport: missing critic report")
	}
	var b strings.Builder
	b.WriteString("# Gate: post-Critic\n\n")
	fmt.Fprintf(&b, "## Issue %s/%s#%d: %s\n\n", cc.Issue.Owner, cc.Issue.Repo, cc.Issue.Number, cc.Issue.Title)
	b.WriteString(cc.Issue.Body)
	b.WriteString("\n\n## Critic report\n\n")
	b.WriteString(cc.CriticReport)
	if cc.ClarifierNotes != "" {
		b.WriteString("\n\n## Clarifier session (already collected)\n\n")
		b.WriteString(cc.ClarifierNotes)
	}
	b.WriteString("\n\n## What to look for at this gate\n\n")
	b.WriteString("The Critic has just emitted its build/defer/kill verdict and reasoning. Before the Architect runs (or before the Clarifier interview, if the verdict was unclear/defer), observe:\n\n")
	b.WriteString("- Does the verdict match the body of the report? (E.g. body argues build but verdict is defer.)\n")
	b.WriteString("- Do the sharp questions ask things the issue body or Clarifier already answers?\n")
	b.WriteString("- Are the AGAINST bullets accurate to the issue's actual scope, or strawmen?\n")
	b.WriteString("- If a Clarifier session is shown above, did the Critic incorporate those answers, or is it asking the same questions again?\n\n")
	b.WriteString("Emit OK: if the report is internally consistent, or NOTE: with a severity if you see specific contradictions or missed signals.")
	return co.runObserveGate(ctx, "post-critic", b.String())
}

// ObserveArchitectSketches runs Gate 2. Reads the Architect's
// sketch set and flags two things specifically: sketches that
// are functionally identical to each other (under-explored
// design space) and sketches that ignore the Critic's sharp
// questions or the Clarifier's answers.
func (co *Coordinator) ObserveArchitectSketches(ctx context.Context, cc *Context) ([]GateNote, error) {
	if cc == nil || cc.Issue == nil {
		return nil, errors.New("ObserveArchitectSketches: missing issue")
	}
	if len(cc.Sketches) == 0 {
		return nil, errors.New("ObserveArchitectSketches: no sketches")
	}
	var b strings.Builder
	b.WriteString("# Gate: post-Architect\n\n")
	fmt.Fprintf(&b, "## Issue %s/%s#%d: %s\n\n", cc.Issue.Owner, cc.Issue.Repo, cc.Issue.Number, cc.Issue.Title)
	b.WriteString(cc.Issue.Body)
	b.WriteString("\n\n## Critic report\n\n")
	b.WriteString(cc.CriticReport)
	if cc.ClarifierNotes != "" {
		b.WriteString("\n\n## Clarifier session\n\n")
		b.WriteString(cc.ClarifierNotes)
	}
	b.WriteString("\n\n## Architect sketches\n\n")
	for _, s := range cc.Sketches {
		fmt.Fprintf(&b, "### Sketch %d: %s\n\n%s\n\n", s.Number, s.Title, s.Markdown)
	}
	b.WriteString("\n\n## What to look for at this gate\n\n")
	b.WriteString("The Architect has just produced its sketch set. Before the Implementer runs against any of them, observe:\n\n")
	b.WriteString("- Are the sketches meaningfully DIFFERENT, or are two/three of them functionally identical with cosmetic variation?\n")
	b.WriteString("- Does each sketch address the Critic's sharp questions and the Clarifier's answers, or do any of them ignore the constraints the user set?\n")
	b.WriteString("- Are any of the sketches obviously infeasible (require migrations outside the repo, contradict themselves, depend on unstated infrastructure)?\n")
	b.WriteString("- Is one sketch clearly better than the others given the constraints, and worth flagging as a recommendation?\n\n")
	b.WriteString("Emit OK: if the sketch set is sound, or NOTE: with severity if you see specific issues with one or more sketches.")
	return co.runObserveGate(ctx, "post-architect", b.String())
}

// ObserveReviewerVerdict runs Gate 5. Reads the Reviewer's
// verdict on the applied patch and flags issues the Reviewer
// missed — informed by the Coordinator's running notes from
// earlier gates (Gate 0/1/2/4 may have already noted things the
// Reviewer should have caught).
func (co *Coordinator) ObserveReviewerVerdict(ctx context.Context, cc *Context, review *Review) ([]GateNote, error) {
	if cc == nil || cc.Issue == nil {
		return nil, errors.New("ObserveReviewerVerdict: missing issue")
	}
	if review == nil {
		return nil, errors.New("ObserveReviewerVerdict: nil review")
	}
	var b strings.Builder
	b.WriteString("# Gate: post-Reviewer\n\n")
	fmt.Fprintf(&b, "## Issue %s/%s#%d: %s\n\n", cc.Issue.Owner, cc.Issue.Repo, cc.Issue.Number, cc.Issue.Title)
	b.WriteString(cc.Issue.Body)
	b.WriteString("\n\n## Reviewer verdict\n\n")
	fmt.Fprintf(&b, "VERDICT: %s\n\n", review.Verdict)
	b.WriteString(review.Markdown)
	if len(co.Notes) > 0 {
		b.WriteString("\n\n## Earlier monitor notes (from this run)\n\n")
		for _, n := range co.Notes {
			fmt.Fprintf(&b, "- [%s/%s] %s\n", n.Gate, n.Severity, oneLineForGate(n.Body))
		}
	}
	b.WriteString("\n\n## What to look for at this gate\n\n")
	b.WriteString("The Reviewer has just verdicted the patch. As the Coordinator who watched the entire run, observe:\n\n")
	b.WriteString("- Does the Reviewer's verdict line up with the concerns earlier monitor gates raised?\n")
	b.WriteString("- Did the Reviewer miss any class of issue (test coverage, syntax validity, scope drift, missing call sites) that you flagged earlier?\n")
	b.WriteString("- If the Reviewer approved, are there any earlier 'concern' notes that the user should still see in the headless report?\n\n")
	b.WriteString("Emit OK: if the Reviewer's pass is consistent with everything you observed, or NOTE: with severity if you see gaps the user should know about.")
	return co.runObserveGate(ctx, "post-reviewer", b.String())
}

// oneLineForGate is a tiny helper that collapses a gate note's
// body to a single line for inclusion in the running notes
// header passed to Gate 5. We don't want a long bullet list of
// earlier notes to dominate the post-Reviewer prompt.
func oneLineForGate(body string) string {
	body = strings.TrimSpace(body)
	if i := strings.IndexAny(body, "\r\n"); i >= 0 {
		return body[:i] + "..."
	}
	if len(body) > 200 {
		return body[:200] + "..."
	}
	return body
}
