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
	RoleImplementer Role = "implementer"
	RoleReviewer    Role = "reviewer"
	RoleTester      Role = "tester"
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
	// Complete performs a single non-streaming completion. Streaming will be
	// added when the TUI needs it; for now simplicity wins.
	Complete(ctx context.Context, req Request) (Response, error)
}
