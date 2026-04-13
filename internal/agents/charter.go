package agents

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aisu-ai/aidev/internal/llm"
)

// Charter is the v0.2c agent that interviews the user about a target
// repository when the repo has no strong signals for the Critic to anchor
// against (no README, no CLAUDE.md, no ARCHITECTURE.md). It asks a short
// fixed set of questions, passes the answers to the large tier, and
// writes the synthesised result to `.aidev/charter.md` inside the target
// repo.
//
// Unlike Scout/Critic/Architect, Charter has a small amount of
// user-facing I/O — it needs to read question answers from somewhere.
// We abstract that behind a `Interviewer` interface so the subcommand
// can use stdin and the unit tests can inject a scripted answerer. The
// LLM call itself is a single Complete() just like every other agent.
type Charter struct {
	Provider    llm.Provider
	Interviewer Interviewer
}

// Interviewer is the side channel Charter uses to ask the human
// questions. A stdin-backed implementation lives in this package; tests
// use a scripted version.
type Interviewer interface {
	Ask(prompt string) (string, error)
}

// NewCharter builds a Charter using the router's RoleCharter mapping
// (defaults to the large tier in the shipped config). If inter is nil,
// a stdin-backed interviewer is used.
func NewCharter(router *llm.Router, inter Interviewer) (*Charter, error) {
	p, err := router.For(llm.RoleCharter)
	if err != nil {
		return nil, err
	}
	if inter == nil {
		inter = &StdinInterviewer{}
	}
	return &Charter{Provider: p, Interviewer: inter}, nil
}

// ChartQuestion is one question in the interview. Exported so tests can
// introspect the fixed set.
type ChartQuestion struct {
	Key    string // short identifier for the field, used in the synthesis prompt
	Prompt string // human-facing wording
}

// CharterQuestions is the fixed set of interview questions. Keep this
// list small and stable — the synthesis prompt relies on the keys.
var CharterQuestions = []ChartQuestion{
	{Key: "purpose", Prompt: "In one sentence, what does this product do?"},
	{Key: "users", Prompt: "Who uses it?"},
	{Key: "constraint", Prompt: "What's the single most important constraint? (correctness / latency / cost / security / compliance / something else)"},
	{Key: "out_of_scope", Prompt: "What is explicitly out of scope — things this product should NOT do?"},
	{Key: "overrides", Prompt: "Any engineering principles that override or extend the defaults? (free text; leave blank for none)"},
}

// Interview runs the full interactive flow: ask all questions via the
// configured Interviewer, synthesise the charter via the LLM, and
// write the result to `<repoRoot>/.aidev/charter.md`. Returns the path
// to the written file.
//
// For non-interactive callers (Claude Code slash commands, CI jobs),
// use RunWithAnswers instead — Interview blocks on stdin and will hang
// forever if stdin is not a TTY.
func (c *Charter) Interview(ctx context.Context, repoRoot string) (string, error) {
	if repoRoot == "" {
		return "", errors.New("charter: empty repo root")
	}
	answers, err := c.askAll()
	if err != nil {
		return "", err
	}
	return c.RunWithAnswers(ctx, repoRoot, answers)
}

// RunWithAnswers is the non-interactive path: it skips the Interviewer
// entirely and goes straight from pre-collected answers to LLM
// synthesis + file write. Used by `aidev charter --answers-file <path>`
// and by the Claude Code `/aidev-charter` slash command, which collects
// answers from the user in the chat before handing them to aidev.
//
// The `answers` map must contain the five keys listed in
// CharterQuestions; an empty `overrides` is allowed. Missing required
// keys return a validation error BEFORE the LLM call.
func (c *Charter) RunWithAnswers(ctx context.Context, repoRoot string, answers map[string]string) (string, error) {
	if repoRoot == "" {
		return "", errors.New("charter: empty repo root")
	}
	if answers == nil {
		return "", errors.New("charter: nil answers")
	}
	// Validate that every required question has a non-empty answer.
	// Missing keys are treated the same as empty strings.
	for _, q := range CharterQuestions {
		v := strings.TrimSpace(answers[q.Key])
		answers[q.Key] = v
		if v == "" && q.Key != "overrides" {
			return "", fmt.Errorf("charter: %q is required", q.Key)
		}
	}
	md, err := c.synthesise(ctx, answers)
	if err != nil {
		return "", err
	}
	return c.writeCharter(repoRoot, md)
}

// askAll iterates the question list and reads each answer. An empty
// answer is allowed (for the optional "overrides" question) but the
// earlier required questions force a non-empty response.
func (c *Charter) askAll() (map[string]string, error) {
	out := make(map[string]string, len(CharterQuestions))
	for _, q := range CharterQuestions {
		answer, err := c.Interviewer.Ask(q.Prompt + " ")
		if err != nil {
			return nil, fmt.Errorf("charter: read %s: %w", q.Key, err)
		}
		answer = strings.TrimSpace(answer)
		if answer == "" && q.Key != "overrides" {
			return nil, fmt.Errorf("charter: %q is required", q.Key)
		}
		out[q.Key] = answer
	}
	return out, nil
}

// synthesise calls the LLM to turn the raw answers into a tight,
// well-formed charter document. The system prompt constrains the output
// to a deterministic section structure so that Scout can parse it
// consistently on future runs.
func (c *Charter) synthesise(ctx context.Context, answers map[string]string) (string, error) {
	system := `You are the Charter writer for aidev, a multi-agent coding tool.

Take the user's raw answers to the interview questions and produce a
clean, publishable charter in Markdown with EXACTLY this structure:

# Charter

## Purpose
One paragraph, 2-4 sentences. Answer "what does this product do?"

## Users
One paragraph. Answer "who uses it, what do they care about?"

## Dominant constraint
One sentence. The single axis that dominates trade-offs.

## Out of scope
Bulleted list. Things the product will not do, even if possible.

## Overriding principles
Bulleted list. Engineering principles that extend or override the
defaults. Use an empty list if the user gave no overrides.

Rules:
- Do NOT invent content the user did not provide. If an answer is
  thin, keep the corresponding section thin.
- Do NOT editorialise or add a preamble/conclusion.
- Do NOT wrap the output in a code fence.
- The first line of your response MUST be "# Charter".`

	var user strings.Builder
	user.WriteString("Interview answers:\n\n")
	for _, q := range CharterQuestions {
		fmt.Fprintf(&user, "**%s**: %s\nuser answer: %s\n\n", q.Key, q.Prompt, answers[q.Key])
	}

	resp, err := c.Provider.Complete(ctx, llm.Request{
		System: system,
		Messages: []llm.Message{
			{Role: "user", Content: user.String()},
		},
	})
	if err != nil {
		return "", fmt.Errorf("charter synthesis: %w", err)
	}
	md := strings.TrimSpace(resp.Content)
	if !strings.HasPrefix(md, "# Charter") {
		return "", fmt.Errorf("charter synthesis: expected '# Charter' header, got %q", firstLine(md))
	}
	return md + "\n", nil
}

// writeCharter writes the charter to `<repoRoot>/.aidev/charter.md`,
// creating the `.aidev` directory if needed. A footer stamp records when
// aidev wrote the file so staleness checks on subsequent runs can cite
// a real date.
func (c *Charter) writeCharter(repoRoot, md string) (string, error) {
	dir := filepath.Join(repoRoot, ".aidev")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("charter: mkdir .aidev: %w", err)
	}
	path := filepath.Join(dir, "charter.md")
	body := md + "\n---\n\n_Generated by aidev on " + time.Now().UTC().Format("2006-01-02") + ". Edit freely._\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return "", fmt.Errorf("charter: write: %w", err)
	}
	return path, nil
}

// StdinInterviewer reads answers from os.Stdin. Prompts are written to
// os.Stderr so they don't taint stdout-piped output.
//
// Ask fails fast if stdin isn't a TTY. This guards against the
// 'slash command invokes aidev charter via !shell execution and aidev
// hangs forever waiting for stdin' failure mode. Callers that need a
// non-interactive path should use Charter.RunWithAnswers with a
// pre-collected map instead of this interviewer.
//
// Implementation note: this uses bufio.Reader.ReadString('\n') to
// read whole lines — NOT fmt.Fscanln, which reads whitespace-delimited
// tokens and silently tokenises a multi-word answer into the first
// word only, leaving the rest of the sentence in stdin to pollute the
// next question. That bug produced garbage charters when users typed
// answers containing spaces (i.e. almost all answers). A single
// bufio.Reader is held on the struct so answers with embedded tabs,
// colons, or other shell-special characters survive.
type StdinInterviewer struct {
	reader *bufio.Reader
}

// Ask prints the prompt to stderr and reads a full line from stdin.
// Returns an error immediately if stdin is not a TTY.
func (s *StdinInterviewer) Ask(prompt string) (string, error) {
	// Non-TTY stdin means this process was invoked from a script, a
	// pipe, or a tool like Claude Code's `!` shell execution. None of
	// those have a user to type answers. Fail fast instead of
	// blocking on ReadString.
	if fi, err := os.Stdin.Stat(); err == nil {
		if (fi.Mode() & os.ModeCharDevice) == 0 {
			return "", errors.New("charter: stdin is not a TTY — use `aidev charter --answers-file <path>` for non-interactive runs")
		}
	}
	fmt.Fprint(os.Stderr, prompt)
	if s.reader == nil {
		s.reader = bufio.NewReader(os.Stdin)
	}
	line, err := s.reader.ReadString('\n')
	// EOF after some input is fine — the user pressed ctrl-d with a
	// line in the buffer. Return what we got. A hard error (e.g.
	// stdin closed mid-read with no bytes) bubbles up so the caller
	// can complain.
	if err != nil && err.Error() != "EOF" {
		return "", fmt.Errorf("charter: read line: %w", err)
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// firstLine returns the first line of s, used only in error messages.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
