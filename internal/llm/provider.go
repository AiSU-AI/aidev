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

import "context"

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
