package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/aisu-ai/aidev/internal/llm"
)

// Triage is the meta-judgment step in aidev's autonomous review→fix loop
// (P6). It runs after the Reviewer agent emits findings and after CI
// reports check status, and produces structured per-finding actions the
// slash command (Claude Code) can execute: fix_now, rebut,
// defer_to_followup, or escalate.
//
// The agent's load-bearing safety property is "rebuttals must be
// grounded in literal Sketch citations". The LLM is told to default to
// escalate when it cannot quote the chosen Sketch's text. Go-side
// post-processing then enforces this hard:
//
//   - rebut without sketch_citation → coerced to escalate
//   - rebut whose sketch_citation does not appear in the chosen Sketch's
//     markdown → coerced to escalate
//   - fix_now whose fix_plan touches a protected path → coerced to
//     escalate
//   - rebut on a security check (CodeQL, Snyk, Dependabot, etc.) →
//     coerced to escalate; security findings are never auto-rebutted
//   - finding ID seen with action=fix_now in a prior round → coerced to
//     escalate (cycle protection)
//
// The LLM's confidence is ONE input among many. When the LLM and the
// safety rules disagree, the safety rules win.
type Triage struct {
	Provider llm.Provider
}

// NewTriage builds a Triage agent from the router. Falls back to the
// Critic tier when RoleTriage is not explicitly routed (see router.go).
func NewTriage(router *llm.Router) (*Triage, error) {
	p, err := router.For(llm.RoleTriage)
	if err != nil {
		return nil, err
	}
	return &Triage{Provider: p}, nil
}

// CIStatus captures GitHub Actions / CI check state for the PR. Pass nil
// when CI is not being consulted (slash command opted out via
// review.wait_for_ci: false, or CI hasn't been wired into this loop
// invocation yet).
type CIStatus struct {
	Checks []CICheck
}

// CICheck is one required check on the PR. Conclusion is "" while
// State is not "completed".
type CICheck struct {
	Name       string // "CodeQL", "build", "test", "lint"
	State      string // "completed", "in_progress", "queued"
	Conclusion string // "success", "failure", "cancelled", "skipped", ""
	DetailsURL string
	LogExcerpt string // log tail when conclusion=failure (slash command captures via `gh run view --log-failed`)
}

// TriageInput packages the inputs the Triage agent reasons over. Built
// by the orchestrator (or the aidev review CLI) before calling Run.
type TriageInput struct {
	Review         *Review        // mandatory; Review.Markdown + parsed Blockers/Suggests/FollowUps
	Patch          string         // mandatory; the diff under review
	CI             *CIStatus      // optional; nil means "CI signal not consulted this round"
	Round          int            // 1-indexed; round number in the review loop
	PrevActions    []TriageAction // actions taken in prior rounds (for cycle protection)
	ProtectedPaths []string       // glob patterns; matches force escalate (defaults applied if empty)
}

// TriageAction is one per-finding judgment. Sources can be "reviewer"
// (from the aidev Reviewer agent) or "ci" (from a GitHub Actions check
// failure).
type TriageAction struct {
	ID             string `json:"id"`
	Source         string `json:"source"`             // "reviewer" | "ci"
	CICheck        string `json:"ci_check,omitempty"` // populated when Source=="ci"
	Severity       string `json:"severity"`           // "blocker" | "suggestion" | "nit" | "followup"
	Finding        string `json:"finding"`            // verbatim from Reviewer or CI log
	Action         string `json:"action"`             // "fix_now" | "rebut" | "defer_to_followup" | "escalate"
	Rationale      string `json:"rationale"`          // one-line WHY this action
	FixPlan        string `json:"fix_plan,omitempty"` // populated when Action=="fix_now"
	RebutText      string `json:"rebut_text,omitempty"`
	SketchCitation string `json:"sketch_citation,omitempty"` // REQUIRED when Action=="rebut"
}

// TriageVerdict is the structured output the slash command consumes.
// Convergence drives the loop's next step (see plan §6d).
type TriageVerdict struct {
	Round          int            `json:"round"`
	ReviewVerdict  string         `json:"review_verdict"` // "approve" | "changes_requested" | "comment"
	Convergence    string         `json:"convergence"`    // "approve" | "rebut_to_ship" | "continue" | "escalate"
	Actions        []TriageAction `json:"actions"`
	CoercionsLog   []string       `json:"coercions_log,omitempty"` // Go-side coercions that overrode the LLM (audit trail)
	EscalateReason string         `json:"escalate_reason,omitempty"`
}

// DefaultProtectedPaths lists glob patterns whose touched files always
// force escalate. Conservative defaults — repo can extend via
// .aidev/aidev.yaml `review.protected_paths`.
var DefaultProtectedPaths = []string{
	"**/migrations/**",
	"*.sql",
	"**/.github/workflows/**",
	"**/secrets/**",
	"*.env",
	"*.env.*",
}

// securityCheckRe matches CI check names that are security tooling. Findings
// from these checks are NEVER auto-rebutted (forced to fix_now or escalate).
// Substring match, case-insensitive.
var securityCheckPatterns = []string{
	"codeql",
	"snyk",
	"dependabot",
	"trivy",
	"semgrep",
	"sonar",
	"npm audit",
	"pnpm audit",
	"yarn audit",
	"osv-scanner",
	"ossf scorecard",
	"socket security",
}

// Run executes the Triage step end-to-end: assembles the LLM prompt,
// parses JSON output, applies safety coercions, and computes
// convergence. Returns a verdict the slash command can act on.
func (t *Triage) Run(ctx context.Context, c *Context, in TriageInput) (*TriageVerdict, error) {
	if c == nil || c.Issue == nil {
		return nil, errors.New("triage: missing issue")
	}
	if in.Review == nil {
		return nil, errors.New("triage: missing review")
	}
	if c.Selector == nil || c.Selector.ChosenNumber == 0 {
		return nil, errors.New("triage: no chosen sketch — Selector verdict required")
	}
	if c.Selector.ChosenNumber > len(c.Sketches) {
		return nil, fmt.Errorf("triage: chosen sketch %d out of range (have %d)", c.Selector.ChosenNumber, len(c.Sketches))
	}
	chosenSketch := c.Sketches[c.Selector.ChosenNumber-1]

	protected := in.ProtectedPaths
	if len(protected) == 0 {
		protected = DefaultProtectedPaths
	}

	// Build the prompt. The LLM gets the chosen Sketch verbatim so it
	// can quote sections, the Reviewer report verbatim so finding
	// language is preserved, and a summary of CI failures (when
	// present) so it can produce CI-source actions.
	prompt := buildTriagePrompt(c, chosenSketch, in)

	resp, err := t.Provider.Complete(ctx, llm.Request{
		System: triageSystemPrompt,
		Messages: []llm.Message{
			{Role: "user", Content: prompt},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("triage: LLM call: %w", err)
	}

	verdict, parseErr := parseTriageJSON(resp.Content)
	if parseErr != nil {
		// Parse failure is treated as escalation, not a hard error: the
		// loop should make forward progress (escalate to a human) when
		// the model returns malformed JSON, not crash the pipeline.
		return &TriageVerdict{
			Round:          in.Round,
			ReviewVerdict:  in.Review.Verdict,
			Convergence:    "escalate",
			Actions:        nil,
			EscalateReason: fmt.Sprintf("triage: could not parse LLM JSON output: %v", parseErr),
		}, nil
	}
	verdict.Round = in.Round
	verdict.ReviewVerdict = in.Review.Verdict

	// Apply safety coercions in this order: protected paths force
	// escalate, security CI rebuts are forbidden, ungrounded rebuts
	// fall back to escalate, cycle protection trips on repeat fix_now
	// IDs. Each coercion appends to CoercionsLog for the audit trail.
	verdict.Actions, verdict.CoercionsLog = applyTriageCoercions(verdict.Actions, chosenSketch, protected, in.PrevActions)

	verdict.Convergence = computeConvergence(verdict)
	if verdict.Convergence == "escalate" && verdict.EscalateReason == "" {
		verdict.EscalateReason = summariseEscalateReason(verdict.Actions, verdict.CoercionsLog)
	}

	return verdict, nil
}

// triageSystemPrompt is the agent's role definition. Kept as a package
// var so tests can introspect / future tweaks land in one place.
var triageSystemPrompt = `You are the Triage agent for aidev's autonomous review→fix loop.

Your job: judge each finding from the Reviewer (and any failing CI check) and decide ONE of four actions per finding:

  - fix_now              The finding is real, actionable, and Claude Code can apply a code edit. Provide a fix_plan naming the files/lines to edit.
  - rebut                The finding contradicts the chosen Sketch's documented decisions. You MUST quote the relevant Sketch section verbatim in sketch_citation. If you cannot cite a Sketch section, this action is NOT available — choose escalate instead.
  - defer_to_followup    The finding is real but out of scope for this PR. The slash command will file it as a separate GH issue.
  - escalate             Human required. Use when: the finding is unclear; the fix would touch protected paths; the finding is security-related and cannot be safely auto-fixed; you cannot ground a rebuttal in the Sketch.

CRITICAL RULES (the Go harness will override your output if you violate these):

  1. rebut WITHOUT a literal sketch_citation = escalate. No exceptions. Do not generate confident-sounding rebuttals from your own judgment.
  2. Security findings (source=ci with check name like CodeQL, Snyk, Dependabot, Trivy, etc.) CANNOT be rebutted. Choose fix_now or escalate.
  3. Fixes that touch protected paths (migrations, SQL, CI workflows, secrets, .env files) MUST be escalate, not fix_now.
  4. If the same finding ID appears in this round AND was action=fix_now in a prior round, force escalate (cycle protection).

OUTPUT: a single JSON object, no surrounding prose. Schema:

{
  "actions": [
    {
      "id": "<stable id derived from the finding text>",
      "source": "reviewer" | "ci",
      "ci_check": "<check name when source=ci>",
      "severity": "blocker" | "suggestion" | "nit" | "followup",
      "finding": "<verbatim finding text>",
      "action": "fix_now" | "rebut" | "defer_to_followup" | "escalate",
      "rationale": "<one short sentence>",
      "fix_plan": "<files/lines to edit, only when action=fix_now>",
      "rebut_text": "<PR-comment-ready rebuttal, only when action=rebut>",
      "sketch_citation": "<verbatim Sketch text that justifies the rebut, only when action=rebut>"
    }
  ]
}

Do not include convergence — the harness computes it. Do not wrap the JSON in markdown code fences.`

// buildTriagePrompt assembles the user-message prompt: Sketch markdown,
// Critic report, Reviewer findings, patch summary, CI status, prior-round
// actions for cycle protection.
func buildTriagePrompt(c *Context, chosenSketch Sketch, in TriageInput) string {
	var b strings.Builder

	fmt.Fprintf(&b, "## Issue\n\n%s/%s#%d — %s\n\n",
		c.Issue.Owner, c.Issue.Repo, c.Issue.Number, c.Issue.Title)

	b.WriteString("## Chosen Sketch (you may cite from this verbatim in sketch_citation)\n\n")
	b.WriteString(chosenSketch.Markdown)
	b.WriteString("\n\n")

	if c.CriticReport != "" {
		b.WriteString("## Critic report (background)\n\n")
		b.WriteString(c.CriticReport)
		b.WriteString("\n\n")
	}

	b.WriteString("## Reviewer findings (verdict: ")
	b.WriteString(in.Review.Verdict)
	b.WriteString(")\n\n")
	b.WriteString(in.Review.Markdown)
	b.WriteString("\n\n")

	if in.CI != nil && len(in.CI.Checks) > 0 {
		b.WriteString("## CI checks\n\n")
		for _, ck := range in.CI.Checks {
			fmt.Fprintf(&b, "- **%s** — state=%s conclusion=%s\n",
				ck.Name, ck.State, ck.Conclusion)
			if ck.Conclusion == "failure" && ck.LogExcerpt != "" {
				b.WriteString("  ```\n")
				b.WriteString(indentLines(strings.TrimSpace(ck.LogExcerpt), "  "))
				b.WriteString("\n  ```\n")
			}
		}
		b.WriteString("\n")
	}

	if len(in.PrevActions) > 0 {
		b.WriteString("## Prior-round actions (cycle protection — do NOT propose fix_now for any ID listed below as having a prior fix_now attempt)\n\n")
		for _, pa := range in.PrevActions {
			fmt.Fprintf(&b, "- id=%s  prev_action=%s\n", pa.ID, pa.Action)
		}
		b.WriteString("\n")
	}

	fmt.Fprintf(&b, "## Round\n\n%d\n\n", in.Round)

	b.WriteString("## Patch under review\n\n```diff\n")
	b.WriteString(in.Patch)
	b.WriteString("\n```\n")

	return b.String()
}

func indentLines(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}

// parseTriageJSON extracts the actions array from the LLM response.
// Tolerates surrounding markdown fences in case the model ignores the
// "no fences" instruction.
func parseTriageJSON(raw string) (*TriageVerdict, error) {
	cleaned := strings.TrimSpace(raw)
	cleaned = stripFences(cleaned)

	var partial struct {
		Actions []TriageAction `json:"actions"`
	}
	if err := json.Unmarshal([]byte(cleaned), &partial); err != nil {
		return nil, err
	}
	return &TriageVerdict{Actions: partial.Actions}, nil
}

func stripFences(s string) string {
	if !strings.HasPrefix(s, "```") {
		return s
	}
	// Find the first newline (skip the fence header) and the last
	// triple-backtick (skip the fence footer).
	first := strings.Index(s, "\n")
	last := strings.LastIndex(s, "```")
	if first < 0 || last <= first {
		return s
	}
	return strings.TrimSpace(s[first+1 : last])
}

// applyTriageCoercions enforces the safety rules the LLM may have
// violated. Returns the coerced actions plus an audit-trail log of
// what was changed and why.
func applyTriageCoercions(actions []TriageAction, chosenSketch Sketch, protectedPaths []string, prev []TriageAction) ([]TriageAction, []string) {
	var log []string

	prevFixIDs := make(map[string]bool, len(prev))
	for _, a := range prev {
		if a.Action == "fix_now" {
			prevFixIDs[a.ID] = true
		}
	}

	out := make([]TriageAction, len(actions))
	for i, a := range actions {
		out[i] = a

		// Cycle protection: same finding ID had a fix_now last round.
		if a.Action == "fix_now" && prevFixIDs[a.ID] {
			out[i].Action = "escalate"
			out[i].Rationale = "Cycle protection: this finding had a fix_now attempt in a prior round and re-appeared. Forced escalate."
			out[i].FixPlan = ""
			log = append(log, fmt.Sprintf("coerce id=%s fix_now→escalate (cycle protection)", a.ID))
			continue
		}

		// Security check rebuts are forbidden.
		if a.Action == "rebut" && a.Source == "ci" && isSecurityCheck(a.CICheck) {
			out[i].Action = "escalate"
			out[i].Rationale = "Security findings cannot be auto-rebutted. " + a.Rationale
			out[i].RebutText = ""
			out[i].SketchCitation = ""
			log = append(log, fmt.Sprintf("coerce id=%s ci-security rebut→escalate (check=%q)", a.ID, a.CICheck))
			continue
		}

		// Ungrounded rebuts → escalate.
		if a.Action == "rebut" {
			cit := strings.TrimSpace(a.SketchCitation)
			if cit == "" || !sketchContainsCitation(chosenSketch.Markdown, cit) {
				reason := "Rebut requires a sketch_citation that appears verbatim in the chosen Sketch."
				if cit == "" {
					reason = "Rebut had no sketch_citation."
				}
				out[i].Action = "escalate"
				out[i].Rationale = reason + " " + a.Rationale
				out[i].RebutText = ""
				out[i].SketchCitation = ""
				log = append(log, fmt.Sprintf("coerce id=%s rebut→escalate (ungrounded citation)", a.ID))
				continue
			}
		}

		// Protected-path fixes → escalate.
		if a.Action == "fix_now" {
			if hit, pattern := fixPlanTouchesProtected(a.FixPlan, protectedPaths); hit {
				out[i].Action = "escalate"
				out[i].Rationale = fmt.Sprintf("fix_plan touches protected path matching %q. ", pattern) + a.Rationale
				out[i].FixPlan = ""
				log = append(log, fmt.Sprintf("coerce id=%s fix_now→escalate (protected path: %s)", a.ID, pattern))
				continue
			}
		}
	}

	return out, log
}

func isSecurityCheck(name string) bool {
	low := strings.ToLower(name)
	for _, p := range securityCheckPatterns {
		if strings.Contains(low, p) {
			return true
		}
	}
	return false
}

// sketchContainsCitation checks whether a non-trivial slice of the
// citation appears verbatim in the sketch markdown. We require the
// citation to be at least 12 chars (filters trivially-short strings)
// and to appear as a contiguous substring after whitespace
// normalisation. Strict enough to catch invented citations; loose
// enough to tolerate the model trimming punctuation.
func sketchContainsCitation(sketch, citation string) bool {
	const minCitationLen = 12
	cit := normaliseCitation(citation)
	if len(cit) < minCitationLen {
		return false
	}
	hay := normaliseCitation(sketch)
	return strings.Contains(hay, cit)
}

func normaliseCitation(s string) string {
	s = strings.ToLower(s)
	s = whitespaceRe.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

var whitespaceRe = regexp.MustCompile(`\s+`)

// fixPlanTouchesProtected scans the fix_plan text for any token that
// matches a protected-path glob. We extract candidate file references
// (anything containing a slash or a dot-extension) and test each
// against each pattern.
func fixPlanTouchesProtected(plan string, patterns []string) (bool, string) {
	if plan == "" {
		return false, ""
	}
	tokens := extractPathLikeTokens(plan)
	for _, tok := range tokens {
		for _, pat := range patterns {
			if globMatch(pat, tok) {
				return true, pat
			}
		}
	}
	return false, ""
}

// pathTokenRe matches anything that looks like a file reference in
// free-form prose: word chars + slashes + dots. Loose on purpose.
var pathTokenRe = regexp.MustCompile(`[\w./*-]+`)

func extractPathLikeTokens(s string) []string {
	all := pathTokenRe.FindAllString(s, -1)
	var out []string
	for _, t := range all {
		// Filter to tokens that look like file paths: contain a slash
		// or end with a recognisable file extension.
		if strings.Contains(t, "/") || hasExtension(t) {
			out = append(out, t)
		}
	}
	return out
}

func hasExtension(s string) bool {
	dot := strings.LastIndex(s, ".")
	if dot < 0 || dot == len(s)-1 {
		return false
	}
	ext := s[dot+1:]
	if len(ext) == 0 || len(ext) > 8 {
		return false
	}
	for _, r := range ext {
		if !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

// globMatch handles a small subset of glob syntax used in
// review.protected_paths: '**' matches across path separators, '*'
// matches within one segment, everything else is literal. Sufficient
// for the conservative default patterns; users with more elaborate
// needs can list more patterns rather than asking us to support
// regexes.
func globMatch(pattern, candidate string) bool {
	re := globToRegex(pattern)
	return re.MatchString(candidate)
}

func globToRegex(pattern string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	i := 0
	for i < len(pattern) {
		c := pattern[i]
		switch {
		case c == '*' && i+1 < len(pattern) && pattern[i+1] == '*':
			b.WriteString(".*")
			i += 2
			// Skip a trailing slash after ** so "**/foo" matches "foo"
			// and "a/foo" both, instead of requiring at least one
			// segment before foo.
			if i < len(pattern) && pattern[i] == '/' {
				i++
			}
		case c == '*':
			b.WriteString("[^/]*")
			i++
		case c == '?':
			b.WriteString("[^/]")
			i++
		case c == '.' || c == '+' || c == '(' || c == ')' || c == '[' || c == ']' || c == '{' || c == '}' || c == '^' || c == '$' || c == '|' || c == '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
			i++
		default:
			b.WriteByte(c)
			i++
		}
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String())
}

// computeConvergence collapses the per-action verdict list into one
// loop-driving outcome. Order of precedence: escalate dominates; then
// continue (any fix_now/defer); then rebut_to_ship (only rebuts and
// nothing requiring action); then approve (clean).
func computeConvergence(v *TriageVerdict) string {
	hasEscalate := false
	hasFixNow := false
	hasDefer := false
	hasRebut := false
	for _, a := range v.Actions {
		switch a.Action {
		case "escalate":
			hasEscalate = true
		case "fix_now":
			hasFixNow = true
		case "defer_to_followup":
			hasDefer = true
		case "rebut":
			hasRebut = true
		}
	}
	if hasEscalate {
		return "escalate"
	}
	if hasFixNow || hasDefer {
		return "continue"
	}
	if hasRebut {
		return "rebut_to_ship"
	}
	// No actions OR all actions are no-ops. If the Reviewer approved,
	// we approve too. Otherwise we approve as well since there's
	// nothing to act on — the loop is naturally converged.
	return "approve"
}

func summariseEscalateReason(actions []TriageAction, log []string) string {
	var ids []string
	for _, a := range actions {
		if a.Action == "escalate" {
			ids = append(ids, a.ID)
		}
	}
	if len(ids) == 0 {
		return strings.Join(log, "; ")
	}
	return fmt.Sprintf("Escalated finding IDs: %s", strings.Join(ids, ", "))
}
