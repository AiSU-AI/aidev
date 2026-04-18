package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/aisu-ai/aidev/internal/llm"
)

// Selector picks one Architect sketch to implement, autonomously, by
// scoring each sketch against a SketchRubric and (when scores are
// genuinely close) calling the cloud tier for a rationale-backed
// tie-break. The output is a SelectorVerdict that the orchestrator
// stores on agents.Context.Selector and the GitHub reporter posts
// as an audit-trail comment.
//
// Why deterministic scoring + LLM tie-break (the "Hybrid"):
//
//   - Most picks should be reproducible: re-running the pipeline on
//     the same issue should pick the same sketch unless the rubric
//     or sketches changed. Pure LLM picking violates that.
//   - But a strict point system can't distinguish "Sketch A scores
//     5.0 because of one weighty alignment" from "Sketch B scores
//     4.9 across many small wins, which is actually better." The
//     epsilon-window LLM tie-break captures exactly those cases.
//   - The LLM call is cheap and only fires on close scores, so the
//     latency tax is small in practice.
type Selector struct {
	Provider llm.Provider // claude-cli tier (RoleSelector → falls back to RoleArchitect)
	Rubric   *SketchRubric
}

// NewSelector builds a Selector using the router's mapping for
// RoleSelector (which falls back to RoleArchitect's tier when
// unrouted — see internal/llm/router.go For()). The rubric must be
// non-nil; callers usually load it via LoadDefaultRubric() and
// optionally override with LoadRubricFile(<repo>/.aidev/sketch-rubric.yaml).
func NewSelector(router *llm.Router, rubric *SketchRubric) (*Selector, error) {
	if rubric == nil {
		return nil, errors.New("selector: nil rubric")
	}
	p, err := router.For(llm.RoleSelector)
	if err != nil {
		return nil, err
	}
	return &Selector{Provider: p, Rubric: rubric}, nil
}

// SelectorVerdict captures everything the orchestrator (and the GH
// audit trail) needs to know about a Selector pick: which sketch won,
// why, what the alternatives scored, and whether the LLM tie-breaker
// was consulted. Stored on agents.Context.Selector.
type SelectorVerdict struct {
	// ChosenNumber is the 1-indexed sketch number the Selector
	// picked. Zero means no sketch was picked (all disqualified, or
	// every score fell below MinImplementableScore — the orchestrator
	// should treat zero as "needs refinement").
	ChosenNumber int

	// Score is the chosen sketch's final deterministic score.
	// Cosmetic; the rationale is what humans actually read.
	Score float64

	// AllScores indexes by ChosenNumber-1 = position in cc.Sketches.
	// Disqualified sketches show -inf. A reader can sort this slice
	// to reconstruct the ranking the Selector saw.
	AllScores []float64

	// Breakdowns mirrors AllScores: per-sketch counts of aligned,
	// tension, violation, risks, and any disqualify reason. Powers
	// the audit-trail "all sketch scores" table in the GH comment.
	Breakdowns []SketchBreakdown

	// Disqualified is the list of 1-indexed sketch numbers that hit
	// a hard veto. Same info as the per-sketch DisqualifyReason in
	// Breakdowns; surfaced separately so callers don't have to walk
	// the Breakdowns slice to find them.
	Disqualified []int

	// Rationale is a 1-3 sentence explanation of why the chosen
	// sketch beat the others. Comes from the deterministic scoring
	// summary OR (when the tie-breaker fires) from the LLM call.
	Rationale string

	// TieBreakerUsed is true when the deterministic phase produced
	// scores within Rubric.TieBreakerEpsilon and the LLM was called
	// to disambiguate. Surfaced in the audit trail so reviewers know
	// whether the pick was deterministic or judgment-based.
	TieBreakerUsed bool

	// RubricVersion is the Rubric.Version string at decision time.
	// Bumping the rubric version makes verdicts traceable to the
	// rules they were scored against.
	RubricVersion string
}

// SketchBreakdown is the per-sketch scoring detail used by the audit
// trail and by debug logs. Field names match the comment-table layout
// in the GH reporter so the reporter can stringify directly.
type SketchBreakdown struct {
	SketchNumber      int
	Title             string
	Score             float64
	Aligned           int
	Tension           int
	Violation         int
	Risks             int
	DisqualifyReason  string
}

// Run scores every sketch on cc.Sketches, picks the winner, and
// returns the verdict. cc.Sketches must already be populated (call
// Architect first). cc.Issue is consulted for type labels; nil Issue
// uses the rubric's "default" override.
func (s *Selector) Run(ctx context.Context, cc *Context) (*SelectorVerdict, error) {
	if cc == nil {
		return nil, errors.New("selector: nil context")
	}
	if len(cc.Sketches) == 0 {
		return nil, errors.New("selector: no sketches to score")
	}

	var labels []string
	if cc.Issue != nil {
		labels = cc.Issue.Labels
	}
	override := s.Rubric.OverrideFor(labels)

	verdict := &SelectorVerdict{
		AllScores:     make([]float64, len(cc.Sketches)),
		Breakdowns:    make([]SketchBreakdown, len(cc.Sketches)),
		RubricVersion: s.Rubric.Version,
	}

	type scored struct {
		index    int // 0-based into cc.Sketches
		score    float64
		disqual  bool
	}
	scores := make([]scored, 0, len(cc.Sketches))

	for i, sk := range cc.Sketches {
		breakdown := scoreSketch(sk, s.Rubric, override)
		verdict.Breakdowns[i] = breakdown
		verdict.AllScores[i] = breakdown.Score
		dis := breakdown.DisqualifyReason != ""
		if dis {
			verdict.Disqualified = append(verdict.Disqualified, sk.Number)
		}
		scores = append(scores, scored{index: i, score: breakdown.Score, disqual: dis})
	}

	// Sort survivors by descending score. Disqualified sketches
	// (score = -inf) always sort to the bottom.
	sort.SliceStable(scores, func(a, b int) bool {
		return scores[a].score > scores[b].score
	})

	// All disqualified? Verdict.ChosenNumber stays 0; orchestrator
	// reads that as "needs refinement" and posts the right comment.
	survivors := 0
	for _, sc := range scores {
		if !sc.disqual {
			survivors++
		}
	}
	if survivors == 0 {
		verdict.Rationale = "Every sketch was disqualified by a hard-veto principle. The orchestrator should escalate to human refinement."
		return verdict, nil
	}

	// Top survivor is always the candidate winner.
	winner := scores[0]
	verdict.ChosenNumber = cc.Sketches[winner.index].Number
	verdict.Score = winner.score

	// Below MinImplementableScore: refuse to pick. Orchestrator
	// treats this the same as all-disqualified.
	if winner.score < s.Rubric.MinImplementableScore {
		verdict.ChosenNumber = 0
		verdict.Rationale = fmt.Sprintf(
			"Best sketch (Sketch %d: %s) scored %.2f, below the rubric's MinImplementableScore of %.2f. The orchestrator should escalate to human refinement.",
			cc.Sketches[winner.index].Number,
			cc.Sketches[winner.index].Title,
			winner.score,
			s.Rubric.MinImplementableScore,
		)
		return verdict, nil
	}

	// Tie-break window: if the gap between #1 and #2 is within
	// epsilon, ask the LLM to disambiguate. Single-survivor case
	// skips this — there's no one to tie with.
	if survivors >= 2 && s.Rubric.TieBreakerEpsilon > 0 {
		runnerUp := scores[1]
		if !runnerUp.disqual && winner.score-runnerUp.score < s.Rubric.TieBreakerEpsilon {
			tied := []int{winner.index, runnerUp.index}
			// Include any further sketches inside the epsilon
			// window (e.g. all three within 0.5).
			for k := 2; k < len(scores); k++ {
				if scores[k].disqual {
					break
				}
				if winner.score-scores[k].score >= s.Rubric.TieBreakerEpsilon {
					break
				}
				tied = append(tied, scores[k].index)
			}
			tieIdx, rationale, err := s.tieBreak(ctx, cc, tied)
			if err == nil && tieIdx >= 0 {
				verdict.TieBreakerUsed = true
				verdict.ChosenNumber = cc.Sketches[tieIdx].Number
				verdict.Score = verdict.AllScores[tieIdx]
				verdict.Rationale = rationale
				return verdict, nil
			}
			// Tie-breaker errored — fall through with the
			// deterministic winner. Don't fail the whole gate
			// over a transient LLM hiccup.
		}
	}

	verdict.Rationale = deterministicRationale(cc.Sketches[winner.index], verdict.Breakdowns[winner.index], s.Rubric)
	return verdict, nil
}

// scoreSketch is the deterministic phase: parse the sketch's markdown
// sub-sections (Approach, Trade-offs, Risks, Principle alignment,
// Rough scope), count alignment signals, apply scope penalties, and
// hard-veto on critical-principle violations.
func scoreSketch(sk Sketch, rubric *SketchRubric, override IssueTypeOverride) SketchBreakdown {
	bd := SketchBreakdown{
		SketchNumber: sk.Number,
		Title:        sk.Title,
	}
	sections := parseSketchSections(sk.Markdown)

	if strings.TrimSpace(sk.Markdown) == "" {
		bd.Score = math.Inf(-1)
		bd.DisqualifyReason = "empty sketch body"
		return bd
	}

	alignment := sections["Principle alignment"]
	bd.Aligned, bd.Tension, bd.Violation, bd.DisqualifyReason = countAlignmentSignals(alignment, rubric)
	if bd.DisqualifyReason != "" {
		bd.Score = math.Inf(-1)
		return bd
	}

	risksSection := sections["Risks"]
	bd.Risks = countBullets(risksSection)

	score := 0.0
	score += float64(bd.Aligned) * rubric.Weights.Aligned
	score += float64(bd.Tension) * rubric.Weights.Tension
	score += float64(bd.Violation) * rubric.Weights.Violation

	riskPenalty := float64(bd.Risks) * rubric.Weights.Risk
	if rubric.Weights.RiskCap < 0 && riskPenalty < rubric.Weights.RiskCap {
		riskPenalty = rubric.Weights.RiskCap
	}
	score += riskPenalty

	// Scope penalty from the issue-type override. Parses "files"
	// figures from the Rough scope section — best-effort: if we
	// can't parse it, we don't penalise. Architect output uses
	// phrases like "9 files touched" or "~8 files".
	if override.ScopeFilesPenalty > 0 {
		files := parseFileCount(sections["Rough scope"])
		if files > override.ScopeFilesThreshold {
			excess := files - override.ScopeFilesThreshold
			score -= float64(excess) * override.ScopeFilesPenalty
		}
	}

	bd.Score = score
	return bd
}

// tieBreak asks the cloud tier to pick among the tied sketches.
// Returns the sketch index in cc.Sketches the LLM picked, plus its
// rationale. Errors are surfaced so the caller can fall back to the
// deterministic winner without crashing the gate.
func (s *Selector) tieBreak(ctx context.Context, cc *Context, tiedIdx []int) (int, string, error) {
	if s.Provider == nil {
		return -1, "", errors.New("selector: nil provider")
	}

	system := `You are the Selector for aidev. The Architect produced multiple
candidate sketches and the deterministic rubric ranked them within an
epsilon of each other. Pick the ONE sketch that best matches the
issue's stated intent and the team's engineering principles.

Return ONLY a JSON object on a single line:

	{"chosen": <sketch_number>, "rationale": "1-3 sentences"}

No prose outside the JSON. No code fence. The chosen number MUST be
one of the sketch numbers shown below.`

	var b strings.Builder
	if cc.Issue != nil {
		fmt.Fprintf(&b, "## GitHub issue %s/%s#%d: %s\n\n", cc.Issue.Owner, cc.Issue.Repo, cc.Issue.Number, cc.Issue.Title)
		b.WriteString(cc.Issue.Body)
		b.WriteString("\n\n")
	}
	if len(cc.Principles) > 0 {
		b.WriteString("## Engineering principles\n\n")
		for _, p := range cc.Principles {
			fmt.Fprintf(&b, "- **%s** — %s\n", p.Name, p.Summary)
		}
		b.WriteString("\n")
	}
	b.WriteString("## Tied sketches\n\n")
	allowed := make([]int, 0, len(tiedIdx))
	for _, i := range tiedIdx {
		sk := cc.Sketches[i]
		fmt.Fprintf(&b, "%s\n\n---\n\n", sk.Markdown)
		allowed = append(allowed, sk.Number)
	}
	fmt.Fprintf(&b, "Pick exactly one of: %v\n", allowed)

	resp, err := s.Provider.Complete(ctx, llm.Request{
		System: system,
		Messages: []llm.Message{
			{Role: "user", Content: b.String()},
		},
	})
	if err != nil {
		return -1, "", fmt.Errorf("selector: tie-break: %w", err)
	}

	chosen, rationale, err := parseTieBreakResponse(resp.Content)
	if err != nil {
		return -1, "", fmt.Errorf("selector: tie-break: %w", err)
	}
	for _, i := range tiedIdx {
		if cc.Sketches[i].Number == chosen {
			return i, rationale, nil
		}
	}
	return -1, "", fmt.Errorf("selector: tie-break chose sketch %d, not in tied set %v", chosen, allowed)
}

func parseTieBreakResponse(raw string) (int, string, error) {
	body := strings.TrimSpace(raw)
	// Strip optional code fence.
	body = strings.TrimPrefix(body, "```json")
	body = strings.TrimPrefix(body, "```")
	body = strings.TrimSuffix(body, "```")
	body = strings.TrimSpace(body)
	// Take only the first JSON object.
	if i := strings.Index(body, "{"); i > 0 {
		body = body[i:]
	}
	if j := strings.LastIndex(body, "}"); j >= 0 && j+1 <= len(body) {
		body = body[:j+1]
	}

	var out struct {
		Chosen    int    `json:"chosen"`
		Rationale string `json:"rationale"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		return 0, "", fmt.Errorf("parse tie-break json: %w (raw: %q)", err, raw)
	}
	if out.Chosen == 0 {
		return 0, "", errors.New("tie-break json missing or zero `chosen`")
	}
	return out.Chosen, strings.TrimSpace(out.Rationale), nil
}

// deterministicRationale composes a one-sentence explanation for why
// the deterministic winner won. Surfaced in the audit trail when no
// tie-breaker was needed.
func deterministicRationale(sk Sketch, bd SketchBreakdown, rubric *SketchRubric) string {
	parts := []string{}
	if bd.Aligned > 0 {
		parts = append(parts, fmt.Sprintf("%d principle alignments", bd.Aligned))
	}
	if bd.Tension > 0 {
		parts = append(parts, fmt.Sprintf("%d tensions", bd.Tension))
	}
	if bd.Violation > 0 {
		parts = append(parts, fmt.Sprintf("%d violations", bd.Violation))
	}
	if bd.Risks > 0 {
		parts = append(parts, fmt.Sprintf("%d risks", bd.Risks))
	}
	signals := strings.Join(parts, ", ")
	if signals == "" {
		signals = "no measurable signals"
	}
	return fmt.Sprintf(
		"Sketch %d (%s) scored %.2f under rubric %s: %s. Top deterministic pick.",
		sk.Number, sk.Title, bd.Score, rubric.Version, signals,
	)
}

// parseSketchSections splits a sketch's Markdown body into a map of
// `### <heading>` → body text. Used by the Selector to extract the
// Principle alignment, Risks, and Rough scope sections without
// re-running the Architect parser.
func parseSketchSections(md string) map[string]string {
	out := make(map[string]string)
	lines := strings.Split(md, "\n")

	currentHeading := ""
	var buf strings.Builder
	flush := func() {
		if currentHeading != "" {
			out[currentHeading] = strings.TrimSpace(buf.String())
		}
		buf.Reset()
	}

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "### ") {
			flush()
			currentHeading = strings.TrimSpace(strings.TrimPrefix(trimmed, "### "))
			continue
		}
		if strings.HasPrefix(trimmed, "## ") {
			// Hit the next sketch's H2 — stop.
			flush()
			currentHeading = ""
			continue
		}
		if currentHeading != "" {
			buf.WriteString(line)
			buf.WriteString("\n")
		}
	}
	flush()
	return out
}

// countAlignmentSignals walks the "Principle alignment" section and
// counts aligned/tension/violation marks. Returns disqualifyReason
// non-empty when ANY violation is on a critical principle.
//
// Format the Architect produces (per architect.go prompt):
//
//	- <Principle Name>: aligned — one sentence why
//	- <Principle Name>: tension — one sentence why
//	- <Principle Name>: violation — one sentence why
func countAlignmentSignals(section string, rubric *SketchRubric) (aligned, tension, violation int, disqualifyReason string) {
	for _, raw := range strings.Split(section, "\n") {
		line := strings.TrimSpace(raw)
		if !strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "*") {
			continue
		}
		// Strip leading bullet marker and any markdown emphasis.
		line = strings.TrimLeft(line, "-* \t")
		// The principle name lives between leading `**` (bold) and
		// the colon. Strip emphasis to get the bare name.
		colonIdx := strings.Index(line, ":")
		if colonIdx < 0 {
			continue
		}
		principleName := strings.Trim(strings.TrimSpace(line[:colonIdx]), "*_")
		marker := strings.ToLower(strings.TrimSpace(line[colonIdx+1:]))

		switch {
		case strings.HasPrefix(marker, "aligned"):
			aligned++
		case strings.HasPrefix(marker, "tension"):
			tension++
		case strings.HasPrefix(marker, "violation"):
			violation++
			if rubric.IsCritical(principleName) && disqualifyReason == "" {
				disqualifyReason = fmt.Sprintf(`violates critical principle %q`, principleName)
			}
		case strings.HasPrefix(marker, "neutral"):
			// "neutral" is allowed but counts as nothing.
		}
	}
	return
}

// countBullets counts bullet items (lines starting with "-" or "*")
// in the given section. Used for risk count.
func countBullets(section string) int {
	n := 0
	for _, raw := range strings.Split(section, "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "- ") || strings.HasPrefix(line, "* ") {
			n++
		}
	}
	return n
}

// parseFileCount best-effort extracts the number of files the sketch
// estimates touching from the "Rough scope" section. Looks for the
// first integer immediately followed by " file" or " files". Returns
// 0 when no count is found — the scope penalty just doesn't fire.
//
// Architect tends to write "9 files touched" or "~8 files edited".
func parseFileCount(section string) int {
	matches := fileCountRe.FindStringSubmatch(section)
	if len(matches) < 2 {
		return 0
	}
	n, err := strconv.Atoi(matches[1])
	if err != nil {
		return 0
	}
	return n
}

var fileCountRe = regexp.MustCompile(`(\d+)\s+files?\b`)
