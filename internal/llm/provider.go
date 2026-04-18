// Package llm defines the provider-agnostic interface aidev's agents use to
// talk to language models, plus the concrete Claude and Ollama backends.
//
// The design goals are deliberately narrow:
//
//   - One interface (Provider) covers both local and cloud models.
//   - The Router maps a role name (scout, critic, architect, ...) to a tier
//     (small, medium, large), and a tier to a Provider instance.
//   - Agents never see a raw Provider; they ask the Router for the role they
//     are playing and receive the appropriate one.
//
// This is enough to let us run the Scout locally on Ollama while the Critic
// reaches for Claude, and to swap either side without touching agent code.
package llm

import (
	"context"
	"encoding/json"
	"errors"
)

// ErrToolsNotSupported is returned by a middleware's CompleteWithTools
// method when the wrapped inner provider does not implement
// ToolAwareProvider. Callers that chain middleware around a mixed set of
// backends (cloud Claude which supports tools; Ollama which supports
// tools; claude-cli which currently does not) detect this sentinel with
// errors.Is and fall back to the legacy Complete() path.
//
// This exists because wrapping a ToolAwareProvider in a plain Provider
// middleware would otherwise erase the tool-use capability at the
// interface boundary: a struct that only declares Name()+Complete() does
// not satisfy ToolAwareProvider even if its inner field does. Every
// middleware in this package therefore declares CompleteWithTools and
// forwards to the inner provider when tool use is available, returning
// ErrToolsNotSupported otherwise. The type assertion at the Implementer
// now always succeeds at compile time; the runtime behaviour still
// degrades gracefully for non-tool-aware backends.
var ErrToolsNotSupported = errors.New("llm: provider does not support native tool use")

// Role is a typed string used by agents when requesting a provider from the
// Router. Using constants keeps typo-bugs out of the hot path.
type Role string

const (
	RoleScout       Role = "scout"
	RoleCritic      Role = "critic"
	RoleArchitect   Role = "architect"
	RoleCharter     Role = "charter"
	RoleImplementer Role = "implementer"
	RoleReviewer    Role = "reviewer"
	RoleTester      Role = "tester"
	// RoleCoordinator is the cloud-backed safety net that reviews the
	// Implementer's diff before it hits disk. Wired into the
	// orchestrator's Implement loop at Gate 1 (post-Implementer,
	// pre-WriteTo). Typically routed to the large tier because catching
	// fabrication and invalid-syntax failures is a judgment task — the
	// same reason the Critic and Architect run there.
	RoleCoordinator Role = "coordinator"
	// RoleSelector is the autonomous picker that runs after the
	// Architect produces N sketches: it scores each sketch against a
	// rubric and (on tie-break) consults the cloud tier for a
	// rationale-backed pick. Falls back to RoleArchitect's tier when
	// not explicitly routed, since Selector is a judgment task that
	// wants the same model the Architect already trusts.
	RoleSelector Role = "selector"
)

// Message is a single turn in a conversation. Role is "user" or "assistant".
type Message struct {
	Role    string
	Content string
}

// Request is a completion request. System is the system prompt; Messages is
// the conversation so far; MaxTokens and Temperature override tier defaults
// when non-zero.
type Request struct {
	System      string
	Messages    []Message
	MaxTokens   int
	Temperature float64
}

// Usage reports token counts when the provider exposes them. Zero means
// unknown (common for local models).
type Usage struct {
	InputTokens  int
	OutputTokens int
}

// Response is what a Provider returns from Complete.
type Response struct {
	Content string
	Model   string
	Usage   Usage
}

// Provider is the minimal surface every LLM backend must implement.
type Provider interface {
	// Name is a human-readable identifier ("ollama:qwen2.5-coder:7b").
	Name() string
	// Complete performs a single non-streaming completion — the most
	// common path for aidev's agent code.
	Complete(ctx context.Context, req Request) (Response, error)
}

// Streamer is an optional capability interface that providers can
// implement to expose incremental responses. Callers detect support via
// a type assertion:
//
//	if s, ok := provider.(llm.Streamer); ok {
//		chunks, err := s.Stream(ctx, req)
//		...
//	}
//
// Providers that don't implement Streamer continue to work unchanged —
// callers can fall back to the single-shot Complete path. aidev's
// shipped providers do the following:
//
//	Claude (REST)   — native SSE streaming
//	Ollama          — native /api/chat?stream=true
//	Claude CLI      — falls back to a synthetic single-chunk stream
//	                  (the CLI's --output-format stream-json is more
//	                  complex than it is worth for aidev's needs)
type Streamer interface {
	// Stream performs a streaming completion. The returned channel
	// emits StreamChunks until it is closed (EOF). Any error is
	// delivered on the err return and the channel is closed
	// immediately. Cancelling ctx aborts the stream.
	Stream(ctx context.Context, req Request) (<-chan StreamChunk, error)
}

// StreamChunk is one unit of a streaming response. Providers emit them
// in order; concatenating every Text field in a stream reconstructs
// the same string Complete() would have returned.
type StreamChunk struct {
	// Text is the incremental output from this chunk. May be empty
	// (keep-alive pings, status frames).
	Text string

	// Done is true on the final chunk of a stream. Some providers
	// deliver trailing metadata in the Done chunk; consumers that only
	// care about text can ignore this field.
	Done bool

	// Err is set on the final chunk if the stream ended with an error.
	// Consumers should check this before drawing conclusions from the
	// accumulated Text.
	Err error
}

// ---------------------------------------------------------------------------
// Tool use (v0.4)
// ---------------------------------------------------------------------------
//
// The NEED_FILES loop in the Implementer was a poor-man's simulation of
// tool use built on top of the single-shot Complete() method: the model
// emitted a fake "NEED_FILES: [...]" string, the harness parsed it,
// loaded files, and re-called. Every brittleness we patched in PRs
// #31/#33/#34/#35/#36 was a leaky abstraction in that simulation.
//
// ToolAwareProvider is the real thing. Backends that implement it
// expose a native tool-use loop where the model calls `read_file`,
// `glob`, `grep`, etc. as real tools within a single open LLM session.
// The Implementer stops round-tripping file content through string
// protocols.
//
// Adoption is opt-in per backend via a Go interface type assertion:
//
//	if toolAware, ok := provider.(llm.ToolAwareProvider); ok {
//		// use native tool loop
//	} else {
//		// fall back to NEED_FILES-style Complete path
//	}
//
// Providers that don't implement ToolAwareProvider keep working via
// the legacy path. v0.4 ships with Anthropic REST as the first and
// only ToolAwareProvider; Ollama and claude-cli land in later
// releases.

// ToolDefinition describes a single tool available to the model.
// InputSchema is a JSON Schema object describing the tool's
// parameters — aidev hand-writes these for the handful of tools it
// exposes rather than generating from Go types, because the schemas
// are small and the clarity benefit is worth the duplication.
type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// ToolContentBlock is one element of a ToolMessage's Content. It
// represents a single typed block — text the user/assistant wrote,
// a tool_use call the assistant made, or a tool_result the user
// (harness) produced in response to a tool_use.
//
// The shape is a flattened union: Type discriminates, and only the
// fields relevant to that type are populated. This maps 1:1 to the
// Anthropic Messages API content-block shape and is how we preserve
// enough state for multi-turn tool conversations without rebuilding
// the whole API on top of a simpler type.
type ToolContentBlock struct {
	// Type is "text", "tool_use", or "tool_result".
	Type string

	// Text is populated when Type == "text".
	Text string

	// ToolUseID identifies a tool_use block (when Type == "tool_use")
	// or the tool_use it responds to (when Type == "tool_result").
	// Set by the provider on tool_use blocks; set by the harness on
	// tool_result blocks so the provider can correlate.
	ToolUseID string

	// ToolName is the tool being invoked (when Type == "tool_use").
	ToolName string

	// ToolInput is the JSON object argument to the tool (when Type ==
	// "tool_use"). Opaque to the provider layer — the agent validates
	// and executes it.
	ToolInput json.RawMessage

	// ToolResultContent is the text returned from executing a tool
	// (when Type == "tool_result"). Provider serializes this as the
	// `content` field of the tool_result block.
	ToolResultContent string

	// ToolResultIsError indicates the tool reported a recoverable
	// failure (file not found, grep had no matches, etc.). The model
	// sees this flag and decides whether to retry, try a different
	// tool, or give up. Harness-level errors (auth, transport) are
	// propagated to the caller as Go errors and never become
	// ToolResult blocks.
	ToolResultIsError bool
}

// ToolMessage is one turn in a tool-aware conversation. Unlike the
// simple Message type, Content is a slice of typed blocks because a
// single assistant turn can interleave text and tool_use blocks and a
// single user turn can contain multiple tool_result blocks (one per
// pending tool_use from the prior assistant turn).
type ToolMessage struct {
	Role    string // "user" | "assistant"
	Content []ToolContentBlock
}

// ToolAwareRequest is the full multi-turn tool-use conversation as
// the caller sees it. Messages is the growing history — on the first
// turn it contains just the initial user message; each subsequent
// turn appends the assistant's response AND a user message with the
// computed tool_result blocks.
//
// WorkingDir is the filesystem root the provider should operate in
// when the backend drives its own tool use as a subprocess agent
// (e.g. the claude-cli provider invokes the `claude` binary with
// cmd.Dir = WorkingDir so its built-in Read / Grep / Glob / LS tools
// see the target repo). Providers that transcript tool calls through
// the harness (anthropic, ollama) ignore this field because the
// harness's ToolExecutor already knows the repo root. Empty string
// means "no specific working directory" — subprocess providers
// should fall back to a neutral location (e.g. tmpDir) rather than
// inherit aidev's own cwd.
type ToolAwareRequest struct {
	System      string
	Tools       []ToolDefinition
	Messages    []ToolMessage
	MaxTokens   int
	Temperature float64
	WorkingDir  string
}

// ToolUse is a single pending tool call extracted from a
// ToolAwareResponse. It's a convenience view: the same information
// also lives in the response's raw message blocks (accessible via
// AssistantMessage), but ToolUses is what an iterating harness
// actually wants to range over.
type ToolUse struct {
	ID    string
	Name  string
	Input json.RawMessage
}

// ToolAwareResponse is the return value from CompleteWithTools. On a
// normal turn Content holds the model's text and StopReason is
// "end_turn"; on a tool-use turn Content may be empty and ToolUses
// holds the pending tool calls the harness must execute and feed
// back in the next request.
//
// AssistantMessage is the full assistant turn represented as a
// ToolMessage — the caller appends this verbatim to its own
// ToolAwareRequest.Messages list before adding the user tool_result
// message for the next iteration. This is the key invariant that
// lets the caller manage state in one place (the Messages slice)
// while the provider stays stateless.
type ToolAwareResponse struct {
	Content          string
	ToolUses         []ToolUse
	StopReason       string // "end_turn" | "tool_use" | "max_tokens" | ...
	Model            string
	Usage            Usage
	AssistantMessage ToolMessage
}

// ToolAwareProvider is the optional capability interface for backends
// that support native tool use. Detect support via a type assertion
// the same way you would for Streamer.
type ToolAwareProvider interface {
	Provider
	CompleteWithTools(ctx context.Context, req ToolAwareRequest) (ToolAwareResponse, error)
}
