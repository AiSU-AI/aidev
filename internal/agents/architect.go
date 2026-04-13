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

	system := fmt.Sprintf(`You are the Architect for aidev, a multi-agent coding tool.

The Critic has already approved this proposal. Your job is to produce exactly
%d MEANINGFULLY DIFFERENT solution sketches so the developer can pick the
approach before any code is written.

REQUIREMENTS — every sketch must obey:

1. Each sketch must differ from the others on at least one major axis:
   - data model / persistence layer
   - API shape / extraction boundary
   - build-vs-buy (use a library) / write-it-ourselves
   - MVP / complete / over-built
   - monolith / extracted package
   - synchronous / event-driven
   DO NOT produce three variations of the same idea with different knobs.

2. Every sketch must be realistic for THIS repository. Cite the repository's
   stated purpose and existing conventions. Do not propose a rewrite unless
   the issue explicitly asks for one.

3. Every sketch must honour the engineering principles provided. If a
   sketch breaks a principle, say so explicitly and justify why that trade is
   worth making.

OUTPUT FORMAT — you MUST use this exact structure for each sketch so a
parser can split on the headings:

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

---

## Sketch 2: ...
(same structure)

---

## Sketch 3: ...
(same structure)

End the report with EXACTLY one line:

SKETCHES: %d

Do NOT include code blocks. Do NOT write the implementation. The Implementer
agent will handle code — you are the person sketching on a whiteboard.`, a.N, a.N)

	// Assemble the user message. We include everything the Critic saw plus
	// the Critic's own report so the Architect can cite specific objections
	// that each sketch addresses.
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

	resp, err := a.Provider.Complete(ctx, llm.Request{
		System: system,
		Messages: []llm.Message{
			{Role: "user", Content: user.String()},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("architect: %w", err)
	}

	sketches := ParseSketches(resp.Content)
	if len(sketches) == 0 {
		return nil, fmt.Errorf("architect: produced no parseable sketches. Raw output:\n%s", resp.Content)
	}
	return sketches, nil
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
