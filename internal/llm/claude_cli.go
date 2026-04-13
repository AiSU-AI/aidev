package llm

import (
	"bytes"
	"context"
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
//	--output-format text     plain text (no JSON envelope)
//	--model <name>           optional, forwarded from config when set
//	--append-system-prompt   the system prompt (if Request.System != "")
//
// The user message is streamed on stdin, which avoids argv length limits
// for long prompts (the Architect and Implementer can easily hit 20 KB
// of user content).
//
// Working directory: the command runs from os.TempDir() in a fresh
// ad-hoc subdirectory so the CLI does NOT inherit CLAUDE.md from
// aidev's own checkout (or from wherever aidev happened to be invoked).
// Without this isolation the Critic would silently read aidev's own
// standards file and get confused about which project it's evaluating.
//
// Token counts are not exposed by the CLI, so Usage fields are always
// zero. Callers that need token accounting should use the native
// anthropic provider instead.
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
func NewClaudeCLI(model string, maxTokens int, temperature float64) *ClaudeCLI {
	return &ClaudeCLI{
		bin:         "claude",
		model:       model,
		maxTokens:   maxTokens,
		temperature: temperature,
		timeout:     5 * time.Minute,
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

	args := []string{"--print", "--output-format", "text"}
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

	content := strings.TrimSpace(stdout.String())
	if content == "" {
		return Response{}, errors.New("claude-cli: empty response")
	}
	return Response{
		Content: content,
		Model:   c.model,
		Usage:   Usage{}, // CLI does not expose token counts in --print mode
	}, nil
}

// SetBin is a test hook that lets unit tests point the provider at a
// fake `claude` binary without touching PATH. Never called from
// production code; exported only so claude_cli_test.go in the same
// package can use it.
func (c *ClaudeCLI) SetBin(path string) { c.bin = path }
