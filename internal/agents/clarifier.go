// Clarifier is v0.5c's "adaptive dialogue" agent. It sits between the
// Critic and the Architect and turns the Critic's ambiguities into a
// structured, dependency-aware question graph that the orchestrator
// (or the `aidev clarify` subcommand) can walk in waves: independent
// questions batch, chained questions serialise.
//
// I specifically chose NOT to rewrite the Critic's output format
// (which was the alternative path proposed in the v0.2b sparring
// round). The Critic's adversarial markdown report is worth keeping.
// The Clarifier is a new agent that reads the Critic's output as
// prose and distils its open questions into a graph.
package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/aisu-ai/aidev/internal/llm"
)

// Clarifier is the v0.5c agent.
type Clarifier struct {
	Provider llm.Provider
}

// NewClarifier builds a Clarifier from the router's RoleCritic mapping
// (reuses the critic tier since the task is adjacent — close
// reasoning about the Critic's own output).
func NewClarifier(router *llm.Router) (*Clarifier, error) {
	p, err := router.For(llm.RoleCritic)
	if err != nil {
		return nil, err
	}
	return &Clarifier{Provider: p}, nil
}

// Question is one node in the Clarifier's output graph.
type Question struct {
	ID        string   `json:"id"`         // stable identifier like "q1"
	Text      string   `json:"text"`       // the prompt the user sees
	DependsOn []string `json:"depends_on"` // IDs of questions whose answers gate this one
}

// QuestionGraph is the Clarifier's structured output.
type QuestionGraph struct {
	Questions []Question `json:"questions"`
}

// Answer pairs a Question ID with the user's reply.
type Answer struct {
	ID   string
	Text string
}

// Run asks the Clarifier LLM to produce a question graph for the
// current issue and Critic report. The caller (a subcommand or the
// orchestrator) is responsible for walking the graph in waves and
// collecting answers.
func (c *Clarifier) Run(ctx context.Context, cc *Context) (*QuestionGraph, error) {
	if cc == nil || cc.Issue == nil {
		return nil, errors.New("clarifier: missing issue")
	}
	if cc.CriticReport == "" {
		return nil, errors.New("clarifier: critic report must run first")
	}

	system := "You are the Clarifier for aidev, a multi-agent coding tool.\n" +
		"\n" +
		"The Critic has just produced a report on a proposed change. Your job\n" +
		"is to read that report and the issue body, identify the unresolved\n" +
		"questions whose answers would materially affect the Architect's\n" +
		"sketches, and return them as a STRUCTURED QUESTION GRAPH.\n" +
		"\n" +
		"REQUIREMENTS:\n" +
		"\n" +
		"1. Output ONLY a JSON object with a single top-level field `questions`\n" +
		"   containing an array of question objects. No markdown, no code\n" +
		"   fence, no prose around it. If you want to wrap it, I will tolerate\n" +
		"   a ```json fence.\n" +
		"\n" +
		"2. Each question has exactly three fields:\n" +
		"     - id: a stable identifier like 'q1', 'q2'. Must be unique.\n" +
		"     - text: the question as the developer will see it.\n" +
		"     - depends_on: an array of other question IDs whose answers are\n" +
		"       prerequisites for asking this one. Empty array [] means the\n" +
		"       question is independent and can be asked in the first wave.\n" +
		"\n" +
		"3. A dependency means 'the answer to this earlier question changes\n" +
		"   what the later question should even be'. Do NOT list dependencies\n" +
		"   for questions that are merely related — only for questions that\n" +
		"   would be DIFFERENT or UNNECESSARY given the earlier answer.\n" +
		"\n" +
		"4. Emit no more than 8 questions. If the Critic raised more\n" +
		"   concerns than that, pick the ones that matter most for the\n" +
		"   Architect's choice of approach.\n" +
		"\n" +
		"5. If the Critic report has NO unresolved questions worth asking\n" +
		"   (e.g. the change is trivial or the Critic already recommended\n" +
		"   kill), return a JSON object with an empty questions array.\n" +
		"\n" +
		"Example output (follow this shape exactly):\n" +
		"\n" +
		"{\"questions\":[\n" +
		"  {\"id\":\"q1\",\"text\":\"What latency target matters most?\",\"depends_on\":[]},\n" +
		"  {\"id\":\"q2\",\"text\":\"Is the data partitionable by tenant?\",\"depends_on\":[]},\n" +
		"  {\"id\":\"q3\",\"text\":\"Given q1 and q2, choose a storage backend: Postgres, Redis, or a managed service?\",\"depends_on\":[\"q1\",\"q2\"]}\n" +
		"]}"

	var principles strings.Builder
	for _, p := range cc.Principles {
		fmt.Fprintf(&principles, "- **%s** — %s\n", p.Name, p.Summary)
	}

	user := fmt.Sprintf(
		"## Issue %s/%s#%d: %s\n\n%s\n\n## Critic report\n\n%s\n\n## Engineering principles\n\n%s",
		cc.Issue.Owner, cc.Issue.Repo, cc.Issue.Number, cc.Issue.Title,
		cc.Issue.Body, cc.CriticReport, principles.String(),
	)

	resp, err := c.Provider.Complete(ctx, llm.Request{
		System: system,
		Messages: []llm.Message{
			{Role: "user", Content: user},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("clarifier: %w", err)
	}

	return ParseQuestionGraph(resp.Content)
}

// clarifierJSONRe matches the first {...} block in the response. A
// markdown code fence around the JSON is handled too — we strip leading
// and trailing ``` lines before parsing.
var clarifierJSONRe = regexp.MustCompile(`(?s)\{.*\}`)

// ParseQuestionGraph extracts a QuestionGraph from a raw model
// response. Tolerates markdown code fences and leading/trailing prose
// (as long as exactly one JSON object is present). Validates that:
//
//   - every question has a non-empty id and text
//   - every depends_on ID refers to another question in the same graph
//   - the resulting graph is acyclic
//
// Exported so the subcommand and tests can share it.
func ParseQuestionGraph(raw string) (*QuestionGraph, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("empty clarifier response")
	}
	// Strip a single surrounding code fence if present.
	raw = stripCodeFence(raw)

	var g QuestionGraph
	if err := json.Unmarshal([]byte(raw), &g); err == nil {
		return validateQuestionGraph(&g)
	}

	// Fallback: pull the first JSON object from the body.
	match := clarifierJSONRe.FindString(raw)
	if match == "" {
		return nil, fmt.Errorf("no JSON object in clarifier response: %q", firstLine(raw))
	}
	if err := json.Unmarshal([]byte(match), &g); err != nil {
		return nil, fmt.Errorf("parse clarifier JSON: %w", err)
	}
	return validateQuestionGraph(&g)
}

// validateQuestionGraph enforces structural invariants: unique IDs,
// non-empty text, depends_on references to real questions, and an
// acyclic dependency graph (detected by a simple DFS colouring).
func validateQuestionGraph(g *QuestionGraph) (*QuestionGraph, error) {
	if g == nil {
		return nil, errors.New("nil question graph")
	}
	seen := make(map[string]bool, len(g.Questions))
	for _, q := range g.Questions {
		if q.ID == "" {
			return nil, errors.New("question with empty id")
		}
		if q.Text == "" {
			return nil, fmt.Errorf("question %q has empty text", q.ID)
		}
		if seen[q.ID] {
			return nil, fmt.Errorf("duplicate question id %q", q.ID)
		}
		seen[q.ID] = true
	}
	// Validate depends_on references.
	for _, q := range g.Questions {
		for _, dep := range q.DependsOn {
			if !seen[dep] {
				return nil, fmt.Errorf("question %q depends on unknown id %q", q.ID, dep)
			}
			if dep == q.ID {
				return nil, fmt.Errorf("question %q depends on itself", q.ID)
			}
		}
	}
	// Cycle detection via DFS colouring.
	const (
		white = 0
		gray  = 1
		black = 2
	)
	colour := make(map[string]int, len(g.Questions))
	byID := make(map[string]Question, len(g.Questions))
	for _, q := range g.Questions {
		byID[q.ID] = q
	}
	var visit func(id string) error
	visit = func(id string) error {
		switch colour[id] {
		case gray:
			return fmt.Errorf("cycle detected at question %q", id)
		case black:
			return nil
		}
		colour[id] = gray
		for _, dep := range byID[id].DependsOn {
			if err := visit(dep); err != nil {
				return err
			}
		}
		colour[id] = black
		return nil
	}
	for _, q := range g.Questions {
		if err := visit(q.ID); err != nil {
			return nil, err
		}
	}
	return g, nil
}

// BatchByDependencies walks the question graph in waves: every wave
// contains questions whose dependencies have all been answered in
// earlier waves. Returns a slice of waves where each wave is a slice
// of questions that can be asked concurrently.
//
// This is the core of the "adaptive dialogue" promise: independent
// questions batch, chained questions serialise. The result drives the
// subcommand's interview loop.
func BatchByDependencies(g *QuestionGraph) [][]Question {
	if g == nil || len(g.Questions) == 0 {
		return nil
	}
	byID := make(map[string]Question, len(g.Questions))
	for _, q := range g.Questions {
		byID[q.ID] = q
	}

	answered := make(map[string]bool)
	var waves [][]Question
	remaining := make([]Question, len(g.Questions))
	copy(remaining, g.Questions)

	for len(remaining) > 0 {
		var wave []Question
		var next []Question
		for _, q := range remaining {
			ready := true
			for _, dep := range q.DependsOn {
				if !answered[dep] {
					ready = false
					break
				}
			}
			if ready {
				wave = append(wave, q)
			} else {
				next = append(next, q)
			}
		}
		// Sort the wave by ID so the output is deterministic across
		// map iteration orders.
		sort.Slice(wave, func(i, j int) bool { return wave[i].ID < wave[j].ID })
		if len(wave) == 0 {
			// Shouldn't happen after validateQuestionGraph, but defend
			// against a partially-cycled graph being passed directly.
			return waves
		}
		waves = append(waves, wave)
		for _, q := range wave {
			answered[q.ID] = true
		}
		remaining = next
	}
	return waves
}

// WriteClarifierMarkdown serialises a Clarifier session (graph + the
// answers the user gave) as markdown to `<repoRoot>/.aidev/clarifier.md`.
// The Architect picks this file up on the next run the same way it
// picks up the charter.
func WriteClarifierMarkdown(repoRoot string, g *QuestionGraph, answers []Answer) (string, error) {
	if repoRoot == "" {
		return "", errors.New("clarifier: empty repo root")
	}
	if g == nil {
		return "", errors.New("clarifier: nil graph")
	}
	dir := filepath.Join(repoRoot, ".aidev")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("clarifier: mkdir: %w", err)
	}
	path := filepath.Join(dir, "clarifier.md")

	answerByID := make(map[string]string, len(answers))
	for _, a := range answers {
		answerByID[a.ID] = a.Text
	}

	var b strings.Builder
	b.WriteString("# Clarifier session\n\n")
	fmt.Fprintf(&b, "_Recorded by aidev on %s._\n\n", time.Now().UTC().Format("2006-01-02"))
	waves := BatchByDependencies(g)
	for w, wave := range waves {
		fmt.Fprintf(&b, "## Wave %d\n\n", w+1)
		for _, q := range wave {
			fmt.Fprintf(&b, "### %s — %s\n\n", q.ID, q.Text)
			if len(q.DependsOn) > 0 {
				fmt.Fprintf(&b, "_Depends on: %s_\n\n", strings.Join(q.DependsOn, ", "))
			}
			ans := answerByID[q.ID]
			if ans == "" {
				ans = "(unanswered)"
			}
			fmt.Fprintf(&b, "**Answer:** %s\n\n", ans)
		}
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return "", fmt.Errorf("clarifier: write: %w", err)
	}
	return path, nil
}
