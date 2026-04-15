package agents

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/aisu-ai/aidev/internal/llm"
)

// Architect is the third agent in the pipeline. After the Critic has produced
// a "build" recommendation (and the human has accepted it), the Architect
// generates N divergent solution sketches so the developer can pick the
// approach before any code is written.
//
// The contract is deliberately narrow:
//
//   - Each sketch must be meaningfully DIFFERENT on at least one architectural
//     axis (data model, API shape, extraction boundary, build-vs-buy,
//     MVP-vs-complete, monolith-vs-extracted-package). "Three variations of
//     the same idea" is a failure mode we prompt hard against.
//   - Sketches are Markdown, not code. The Implementer agent (v0.3) is the
//     one that writes code against the chosen sketch.
//   - The parser is strict about section headers so the orchestrator can count
//     them and the TUI can navigate them; freeform output is a bug.
type Architect struct {
	Provider llm.Provider
	N        int // number of sketches to produce; default 3
}

// DefaultSketchCount is the default N when none is specified.
const DefaultSketchCount = 3

// NewArchitect builds an Architect from the router's RoleArchitect mapping
// (typically the large, cloud-hosted tier). n <= 0 falls back to the default.
func NewArchitect(router *llm.Router, n int) (*Architect, error) {
	p, err := router.For(llm.RoleArchitect)
	if err != nil {
		return nil, err
	}
	if n <= 0 {
		n = DefaultSketchCount
	}
	return &Architect{Provider: p, N: n}, nil
}

// Sketch is a single solution proposal. Title is the short name the TUI
// displays in a list; Markdown is the full body for the detail pane.
type Sketch struct {
	Number   int    // 1-indexed
	Title    string // short heading, pulled from the sketch's H2
	Markdown string // full sketch body including the heading
}

// CostEstimate is a rough, pre-flight guess at how much the Architect run
// will cost in tokens and wall time. We keep it coarse on purpose — it is a
// user-facing "is this worth running" signal, not a billing system.
type CostEstimate struct {
	N                  int
	OutputTokensPerSketch int
	TotalOutputTokens  int
	EstimatedSeconds   int
}

// EstimateCost produces a rough cost preview for n sketches at the current
// tier's temperature. The numbers are based on observed output length from
// the shipped prompt: each sketch averages ~600 output tokens, and the large
// tier answers at ~60 output tokens per second.
func EstimateCost(n int) CostEstimate {
	if n <= 0 {
		n = DefaultSketchCount
	}
	const perSketch = 600
	const tokensPerSecond = 60
	return CostEstimate{
		N:                     n,
		OutputTokensPerSketch: perSketch,
		TotalOutputTokens:     perSketch * n,
		EstimatedSeconds:      (perSketch * n) / tokensPerSecond,
	}
}

// Run produces N sketches. It fails loudly if the shared Context is missing
// the Critic's report, because the Architect is only meaningful after the
// build/defer/kill decision has been made.
//
// Each sketch is produced by a separate Complete() call. Splitting the work
// this way has three wins over asking for all N sketches in one response:
//
//   - Smaller context per call → lower SIGKILL / OOM risk on the model side.
//     (Concretely: claude-cli was getting SIGKILL'd on the single big call
//     for N=3; splitting made the problem go away.)
//   - Retry middleware works per sketch — a transient failure on one call
//     no longer takes the other two down with it.
//   - Partial success is possible: if one of N sketches fails retries-
//     exhausted, we still return the ones that succeeded. An all-or-nothing
//     Architect gate was a bad tradeoff for an advisory step.
//
// Serial, not parallel — sequential calls let each sketch see prior titles
// so the "diversity hint" isn't guessing. Parallelism is a latency win we
// can add later once telemetry shows it matters; for now the reliability
// win is the whole story.
func (a *Architect) Run(ctx context.Context, cc *Context) ([]Sketch, error) {
	if cc == nil || cc.Issue == nil {
		return nil, fmt.Errorf("architect: missing issue")
	}
	if cc.ScoutReport == "" {
		return nil, fmt.Errorf("architect: scout report must run first")
	}
	if cc.CriticReport == "" {
		return nil, fmt.Errorf("architect: critic report must run first")
	}

	// The user message is the same for every sketch call — context is
	// immutable across the loop, only the system prompt changes per call
	// to steer diversity.
	userMsg := a.buildUserMessage(cc)

	var sketches []Sketch
	var callErrors []error
	priorTitles := make([]string, 0, a.N)

	for i := 1; i <= a.N; i++ {
		system := a.buildSketchSystemPrompt(i, priorTitles)
		resp, err := a.Provider.Complete(ctx, llm.Request{
			System: system,
			Messages: []llm.Message{
				{Role: "user", Content: userMsg},
			},
		})
		if err != nil {
			// Accumulate the error but keep going — partial success is
			// the whole point of the split. If every call fails we'll
			// surface a combined error below.
			callErrors = append(callErrors, fmt.Errorf("sketch %d: %w", i, err))
			continue
		}
		parsed := ParseSketches(resp.Content)
		if len(parsed) == 0 {
			callErrors = append(callErrors, fmt.Errorf("sketch %d: no parseable sketch in response:\n%s", i, resp.Content))
			continue
		}
		// The per-call prompt always asks for "## Sketch 1: ..." so the
		// model's numbering is relative. Renumber to the loop index and
		// recompute the header so ParseSketches downstream (if anyone
		// re-parses the aggregate) continues to work.
		s := parsed[0]
		s.Number = i
		s.Markdown = renumberSketch(s.Markdown, i)
		sketches = append(sketches, s)
		priorTitles = append(priorTitles, s.Title)
	}

	if len(sketches) == 0 {
		return nil, fmt.Errorf("architect: produced no parseable sketches after %d calls. First error: %w", a.N, firstOrNil(callErrors))
	}
	// Partial success path — log the missing ones via the returned
	// sketches slice (caller sees len(sketches) < N) but do not fail.
	// The headless reporter already prints "Architect produced X
	// sketches" which will naturally reflect any shortfall.
	return sketches, nil
}

// buildUserMessage assembles the per-run user message the Architect sees
// on every sketch call. It contains the issue, the Scout brief, the
// Critic report, and the short-form engineering principles.
func (a *Architect) buildUserMessage(cc *Context) string {
	var principles strings.Builder
	for _, p := range cc.Principles {
		fmt.Fprintf(&principles, "- **%s** — %s\n", p.Name, p.Summary)
	}

	var user strings.Builder
	fmt.Fprintf(&user, "## GitHub issue %s/%s#%d: %s\n\n",
		cc.Issue.Owner, cc.Issue.Repo, cc.Issue.Number, cc.Issue.Title)
	user.WriteString(cc.Issue.Body)
	user.WriteString("\n\n## Scout brief\n\n")
	user.WriteString(cc.ScoutReport)
	user.WriteString("\n\n## Critic report\n\n")
	user.WriteString(cc.CriticReport)
	user.WriteString("\n\n## Engineering principles (short form)\n\n")
	user.WriteString(principles.String())
	return user.String()
}

// buildSketchSystemPrompt produces the per-call system prompt for sketch
// i (1-indexed). When i > 1 it includes a "prior sketches" note so the
// model can explicitly diverge from what already exists.
func (a *Architect) buildSketchSystemPrompt(i int, priorTitles []string) string {
	var diversity string
	if len(priorTitles) > 0 {
		var b strings.Builder
		b.WriteString("\nPRIOR SKETCHES ALREADY PRODUCED (you must be meaningfully different from all of these):\n")
		for idx, title := range priorTitles {
			fmt.Fprintf(&b, "  %d. %s\n", idx+1, title)
		}
		b.WriteString("\nYour sketch must differ from ALL of the above on at least one major axis.\n")
		diversity = b.String()
	}

	return fmt.Sprintf(`You are the Architect for aidev, a multi-agent coding tool.

The Critic has already approved this proposal. Your job is to produce exactly
ONE solution sketch. This is sketch %d of %d for this issue; the developer
will pick one of the %d total sketches to hand to the Implementer.
%s
REQUIREMENTS — your sketch must obey:

1. It must differ from any prior sketches on at least one major axis:
   - data model / persistence layer
   - API shape / extraction boundary
   - build-vs-buy (use a library) / write-it-ourselves
   - MVP / complete / over-built
   - monolith / extracted package
   - synchronous / event-driven
   DO NOT produce a variation of a prior sketch with different knobs.

2. It must be realistic for THIS repository. Cite the repository's stated
   purpose and existing conventions. Do not propose a rewrite unless the
   issue explicitly asks for one.

3. It must honour the engineering principles provided. If it breaks a
   principle, say so explicitly and justify why that trade is worth making.

OUTPUT FORMAT — you MUST use this exact structure so a parser can find it:

## Sketch 1: <short title, 3-6 words>

### Approach
2-4 sentences on what this sketch actually does.

### Key decisions
- bullet: a decision this sketch makes
- bullet: another decision
- bullet: another decision

### Trade-offs
- pro: ...
- pro: ...
- con: ...
- con: ...

### Risks
- bullet

### Principle alignment
- <Principle Name>: aligned / tension / violation — one sentence why

### Rough scope
Estimated files touched, approximate lines of code added or changed, whether
it needs a migration, whether it needs new dependencies.

End with EXACTLY one line:

SKETCHES: 1

ALWAYS use "## Sketch 1:" as the heading even though this is sketch %d of
the full run — the orchestrator renumbers sketches after aggregating them.
Do NOT include code blocks. Do NOT write the implementation. The Implementer
agent will handle code — you are the person sketching on a whiteboard.`,
		i, a.N, a.N, diversity, i)
}

// renumberSketch rewrites a "## Sketch 1: Title" heading to "## Sketch N:
// Title" so the aggregated sketch list has sequential numbers even though
// each per-call response says "Sketch 1".
func renumberSketch(md string, n int) string {
	return sketchHeaderRe.ReplaceAllStringFunc(md, func(match string) string {
		sub := sketchHeaderRe.FindStringSubmatch(match)
		if len(sub) < 3 {
			return match
		}
		return fmt.Sprintf("## Sketch %d: %s", n, strings.TrimSpace(sub[2]))
	})
}

func firstOrNil(errs []error) error {
	if len(errs) == 0 {
		return nil
	}
	return errs[0]
}

// sketchHeaderRe matches a markdown H2 of the form "## Sketch N: Title".
// It is anchored to the start of a line via (?m) so it plays nicely with
// regexp.Split.
var sketchHeaderRe = regexp.MustCompile(`(?m)^## Sketch (\d+):\s*(.*)$`)

// ParseSketches extracts sketches from the Architect's raw Markdown output.
// It is tolerant of extra whitespace, trailing RECOMMENDATION-style lines,
// and missing trailing sketches, but STRICT about the "## Sketch N: Title"
// header format — that is the contract the orchestrator and TUI rely on.
//
// Exported so architect_test.go can exercise it directly.
func ParseSketches(md string) []Sketch {
	matches := sketchHeaderRe.FindAllStringSubmatchIndex(md, -1)
	if len(matches) == 0 {
		return nil
	}
	out := make([]Sketch, 0, len(matches))
	for i, m := range matches {
		// Full submatch indices: 0,1=whole, 2,3=number, 4,5=title
		headerStart := m[0]
		var bodyEnd int
		if i+1 < len(matches) {
			bodyEnd = matches[i+1][0]
		} else {
			bodyEnd = len(md)
		}
		number := md[m[2]:m[3]]
		title := strings.TrimSpace(md[m[4]:m[5]])
		body := strings.TrimSpace(md[headerStart:bodyEnd])
		// Chop off a trailing "SKETCHES: N" tail if the model put it
		// inside the last sketch.
		body = trimSketchesTail(body)
		n := 0
		fmt.Sscanf(number, "%d", &n)
		out = append(out, Sketch{Number: n, Title: title, Markdown: body})
	}
	return out
}

// trimSketchesTail removes a trailing "SKETCHES: N" line and any trailing
// horizontal rule or whitespace from a sketch body.
func trimSketchesTail(body string) string {
	lines := strings.Split(body, "\n")
	for len(lines) > 0 {
		last := strings.TrimSpace(lines[len(lines)-1])
		if last == "" || last == "---" || strings.HasPrefix(strings.ToUpper(last), "SKETCHES:") {
			lines = lines[:len(lines)-1]
			continue
		}
		break
	}
	return strings.Join(lines, "\n")
}
