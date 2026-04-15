package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// ClaudeCLI is a Provider that shells out to the local `claude` CLI
// (Claude Code) instead of calling the Anthropic REST API directly.
// It is the default backend for aidev's large and medium tiers in the
// shipped config: users who already have a Claude Max/Pro subscription
// pay nothing extra to run aidev, because every Critic/Architect/
// Charter/Reviewer call routes through `claude --print`.
//
// The provider invokes `claude` with these flags:
//
//	--print                  one-shot, print response to stdout, exit
//	--output-format json     single JSON envelope with result + usage
//	--model <name>           optional, forwarded from config when set
//	--append-system-prompt   the system prompt (if Request.System != "")
//
// The user message is streamed on stdin, which avoids argv length limits
// for long prompts (the Architect and Implementer can easily hit 20 KB
// of user content).
//
// Output format rationale. The CLI supports three formats:
//
//   - text       — plain assistant text, no metadata. Simple but drops
//     token counts, which breaks per-gate cost visibility in the
//     headless telemetry table.
//   - json       — a single envelope printed after the turn completes,
//     containing `result` (assistant text) and `usage` (input/output
//     token counts). Single-shot, trivial to parse. This is what we use.
//   - stream-json — newline-delimited events (message_start, content
//     deltas, message_stop…). Required if we ever want incremental
//     chunks for a Streamer implementation, but overkill for the
//     one-shot Complete() path because we already block until the
//     subprocess exits.
//
// ClaudeCLI implements only Complete() — there is no Stream() method —
// so the stream-json complexity is not warranted here. If a streaming
// path is added later, revisit this tradeoff for that code path alone;
// the Complete() path should stay on single-envelope json.
//
// Working directory: the command runs from os.TempDir() in a fresh
// ad-hoc subdirectory so the CLI does NOT inherit CLAUDE.md from
// aidev's own checkout (or from wherever aidev happened to be invoked).
// Without this isolation the Critic would silently read aidev's own
// standards file and get confused about which project it's evaluating.
//
// Token counts are parsed from the json envelope's `usage` block and
// surfaced via Response.Usage so telemetry (cost per gate) works for
// claude-cli gates (Critic, Architect, Coordinator, Reviewer) just as
// it does for the native anthropic and ollama providers.
type ClaudeCLI struct {
	// bin is the CLI binary to exec. Resolved via exec.LookPath("claude")
	// at construction time so a missing binary fails loudly at startup
	// rather than at first use.
	bin string

	// model is the optional --model override. Empty string means "use
	// whatever the subscription defaults to", which is the preferred
	// shipped behaviour.
	model string

	// maxTokens and temperature are not actually forwarded to the CLI
	// (it doesn't expose per-call overrides in print mode) but are
	// kept on the struct for parity with the other providers. Listed
	// in the provider Name() so users can tell tiers apart in doctor
	// output.
	maxTokens   int
	temperature float64

	// timeout caps the wall-clock time of a single Complete() call so
	// a hung CLI process can't stall the whole pipeline.
	timeout time.Duration
}

// NewClaudeCLI constructs a ClaudeCLI provider. It does NOT call
// exec.LookPath because doctor does that in its own check; we want
// provider construction to be cheap and lazy. A missing binary will
// surface at the first Complete() call with a clear error message.
//
// timeout may be 0; in that case DefaultHTTPTimeout is used. The
// CLI subprocess timeout doubly bounds a hung 'claude --print' call,
// complementing the context.WithTimeout that runs around exec.
func NewClaudeCLI(model string, maxTokens int, temperature float64, timeout time.Duration) *ClaudeCLI {
	if timeout <= 0 {
		timeout = DefaultHTTPTimeout
	}
	return &ClaudeCLI{
		bin:         "claude",
		model:       model,
		maxTokens:   maxTokens,
		temperature: temperature,
		timeout:     timeout,
	}
}

// Name is the human-readable identifier used in doctor reports and
// provider Name() checks.
func (c *ClaudeCLI) Name() string {
	if c.model != "" {
		return "claude-cli:" + c.model
	}
	return "claude-cli"
}

// Complete runs a single prompt through `claude --print` and returns
// the resulting text. Errors include the CLI's stderr so configuration
// problems (not logged in, model not allowed on this plan, etc.) are
// obvious at the point of failure.
func (c *ClaudeCLI) Complete(ctx context.Context, r Request) (Response, error) {
	if c.bin == "" {
		return Response{}, errors.New("claude-cli: empty bin")
	}

	// Flatten the message list into a single user prompt. aidev's agents
	// currently emit single-turn conversations, so concatenation is
	// lossless; if we ever need multi-turn via the CLI we'll revisit.
	var user strings.Builder
	for _, m := range r.Messages {
		if m.Role != "assistant" {
			user.WriteString(m.Content)
			user.WriteString("\n")
		}
	}
	prompt := strings.TrimSpace(user.String())
	if prompt == "" {
		return Response{}, errors.New("claude-cli: empty user prompt")
	}

	args := []string{"--print", "--output-format", "json"}
	if c.model != "" {
		args = append(args, "--model", c.model)
	}
	if r.System != "" {
		args = append(args, "--append-system-prompt", r.System)
	}

	// Run from a temp dir so the CLI doesn't pick up a local CLAUDE.md.
	tmpDir, err := os.MkdirTemp("", "aidev-claude-cli-")
	if err != nil {
		return Response{}, fmt.Errorf("claude-cli: tmpdir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	runCtx := ctx
	if c.timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(runCtx, c.bin, args...)
	cmd.Dir = tmpDir
	cmd.Stdin = strings.NewReader(prompt)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		msg := stderr.String()
		if msg == "" {
			msg = err.Error()
		}
		// Truncate stderr so the returned error stays readable.
		if len(msg) > 2048 {
			msg = msg[:2048] + "...[truncated]"
		}
		return Response{}, fmt.Errorf("claude-cli: %w (stderr: %s)", err, strings.TrimSpace(msg))
	}

	raw := strings.TrimSpace(stdout.String())
	if raw == "" {
		return Response{}, errors.New("claude-cli: empty response")
	}

	// Parse the single-envelope json emitted by --output-format json.
	// Shape (only fields we care about):
	//
	//	{
	//	  "type": "result",
	//	  "result": "<assistant text>",
	//	  "usage": {
	//	    "input_tokens": 1234,
	//	    "output_tokens": 567,
	//	    ...
	//	  },
	//	  ...
	//	}
	//
	// Failure mode: if the envelope is malformed or missing `result`, we
	// fall back to the raw stdout as Content and leave Usage zero. This
	// preserves backwards compatibility with any CLI version that might
	// not emit the envelope exactly as expected, and keeps Complete()
	// from failing just because a parse hiccup occurred after the CLI
	// already returned successfully.
	var env struct {
		Result string `json:"result"`
		Usage  struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(raw), &env); err != nil || env.Result == "" {
		return Response{
			Content: raw,
			Model:   c.model,
			Usage:   Usage{},
		}, nil
	}
	return Response{
		Content: strings.TrimSpace(env.Result),
		Model:   c.model,
		Usage: Usage{
			InputTokens:  env.Usage.InputTokens,
			OutputTokens: env.Usage.OutputTokens,
		},
	}, nil
}

// SetBin is a test hook that lets unit tests point the provider at a
// fake `claude` binary without touching PATH. Never called from
// production code; exported only so claude_cli_test.go in the same
// package can use it.
func (c *ClaudeCLI) SetBin(path string) { c.bin = path }

// CompleteWithTools implements ToolAwareProvider for ClaudeCLI by
// running the claude CLI binary as a SUBPROCESS AGENT against the
// target repository. This is a fundamentally different shape from
// the anthropic and ollama tool-use loops: those transcript every
// individual tool_use block through aidev's harness. For claude-cli
// we hand the whole task to `claude -p` with its built-in tools
// (Read, Grep, Glob, LS) pre-authorized, let it explore the repo
// autonomously, and capture the final unified diff from its output.
//
// Why not transcript individual tool calls? Because the `claude`
// binary does not expose a tool_use protocol over its --print
// interface — it's a single-shot REPL. Attempting to drive it
// through aidev's legacy NEED_FILES picker failed because claude's
// response shape is "I'll use my tools to read files" followed by
// a reasoning trace, not a JSON file-selection array. Handing it
// the task and letting it use its own tools internally aligns with
// how claude CLI was designed to be used.
//
// Tool scope: only read-only tools are allowed (Read, Grep, Glob,
// LS). Edit / Write / Bash are NOT in --allowedTools so the agent
// cannot modify the repo directly — its only output channel is the
// final stdout text, which aidev captures as a diff candidate. This
// preserves the aidev contract that the user reviews every patch
// before applying.
//
// Working directory: cmd.Dir is set to req.WorkingDir so the agent's
// tools operate inside the target repo. This is the ONE place where
// the claude-cli provider reads the target repo directly; the
// non-tool Complete() path deliberately runs from a neutral tmpDir
// to avoid inheriting an unrelated CLAUDE.md.
//
// The returned ToolAwareResponse always has StopReason="end_turn"
// and no ToolUses — from aidev's perspective this is a single
// synthetic turn that happens to take a long time. The internal
// tool calls claude made are not visible to aidev telemetry.
func (c *ClaudeCLI) CompleteWithTools(ctx context.Context, r ToolAwareRequest) (ToolAwareResponse, error) {
	if c.bin == "" {
		return ToolAwareResponse{}, errors.New("claude-cli: empty bin")
	}

	// Flatten the seed message(s) into a single task prompt. The
	// implementer's runWithTools always seeds with exactly one user
	// message so this concatenation is lossless today; if a future
	// caller sends a multi-turn conversation we'll re-examine.
	var user strings.Builder
	for _, m := range r.Messages {
		if m.Role == "assistant" {
			continue
		}
		for _, block := range m.Content {
			if block.Type == "text" && block.Text != "" {
				user.WriteString(block.Text)
				user.WriteString("\n")
			}
		}
	}
	prompt := strings.TrimSpace(user.String())
	if prompt == "" {
		return ToolAwareResponse{}, errors.New("claude-cli: empty user prompt")
	}

	args := []string{
		"--print",
		"--output-format", "json",
		// Pre-authorize the read-only exploration tools so the
		// subprocess never blocks on a permission prompt. The
		// agent must NOT be allowed to Edit / Write / Bash — its
		// only output is stdout, and aidev captures that as a
		// diff candidate. User reviews every patch before apply.
		"--allowedTools", "Read", "Grep", "Glob", "LS",
	}
	if c.model != "" {
		args = append(args, "--model", c.model)
	}
	if r.System != "" {
		// Append (do not replace) the aidev implementer system
		// prompt on top of claude CLI's default agent prompt.
		// Replacing would strip claude's built-in tool-use guidance.
		args = append(args, "--append-system-prompt", r.System)
	}

	// Unlike Complete(), this path DELIBERATELY runs inside the target
	// repo so claude's tools can read its files. Empty WorkingDir is
	// a configuration error at the caller layer — fail fast rather
	// than silently pointing the agent at aidev's own checkout.
	if strings.TrimSpace(r.WorkingDir) == "" {
		return ToolAwareResponse{}, errors.New("claude-cli: CompleteWithTools requires ToolAwareRequest.WorkingDir — the target repo root")
	}

	runCtx := ctx
	if c.timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(runCtx, c.bin, args...)
	cmd.Dir = r.WorkingDir
	cmd.Stdin = strings.NewReader(prompt)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		msg := stderr.String()
		if msg == "" {
			msg = err.Error()
		}
		if len(msg) > 2048 {
			msg = msg[:2048] + "...[truncated]"
		}
		return ToolAwareResponse{}, fmt.Errorf("claude-cli: agent mode: %w (stderr: %s)", err, strings.TrimSpace(msg))
	}

	raw := strings.TrimSpace(stdout.String())
	if raw == "" {
		return ToolAwareResponse{}, errors.New("claude-cli: agent mode: empty response")
	}

	// Parse the same envelope Complete() parses — agent mode uses
	// the same --output-format json so the `result` + `usage` shape
	// is identical. The `result` field contains the agent's final
	// assistant text, which should be a unified diff.
	var env struct {
		Result string `json:"result"`
		Usage  struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	var content string
	var usage Usage
	if err := json.Unmarshal([]byte(raw), &env); err != nil || env.Result == "" {
		// Fallback: raw stdout as content, zero usage. Matches
		// Complete()'s malformed-envelope behaviour so downstream
		// validators still see something to work with.
		content = raw
	} else {
		content = strings.TrimSpace(env.Result)
		usage = Usage{
			InputTokens:  env.Usage.InputTokens,
			OutputTokens: env.Usage.OutputTokens,
		}
	}

	// Build the synthetic assistant message the harness appends to
	// its history. Only one text block: the final diff candidate.
	// No tool_use blocks because claude ran its tools internally.
	assistant := ToolMessage{
		Role: "assistant",
		Content: []ToolContentBlock{
			{Type: "text", Text: content},
		},
	}

	return ToolAwareResponse{
		Content:          content,
		ToolUses:         nil,
		StopReason:       "end_turn",
		Model:            c.model,
		Usage:            usage,
		AssistantMessage: assistant,
	}, nil
}
