package llm

import (
	"testing"
	"time"
)

// TestNewOllamaAppliesDefaultTimeout verifies the v0.5b behavior:
// when the caller passes 0 for timeout, NewOllama substitutes the
// 20-minute DefaultHTTPTimeout instead of creating a client with
// no timeout (which would be a footgun — a stuck LLM call could
// hang a pipeline indefinitely).
func TestNewOllamaAppliesDefaultTimeout(t *testing.T) {
	p := NewOllama("http://localhost:11434", "test-model", 1024, 0.2, 0)
	if p.http.Timeout != DefaultHTTPTimeout {
		t.Errorf("Timeout = %s, want DefaultHTTPTimeout (%s)", p.http.Timeout, DefaultHTTPTimeout)
	}
}

// TestNewOllamaRespectsExplicitTimeout verifies that a non-zero
// timeout is passed through verbatim. This is the path the
// Router.buildProvider takes when the tier's YAML sets
// timeout_seconds: NNN — the value becomes a time.Duration and
// flows into the HTTP client.
func TestNewOllamaRespectsExplicitTimeout(t *testing.T) {
	want := 45 * time.Minute
	p := NewOllama("http://localhost:11434", "test-model", 1024, 0.2, want)
	if p.http.Timeout != want {
		t.Errorf("Timeout = %s, want %s", p.http.Timeout, want)
	}
}

// TestNewClaudeAppliesDefaultTimeout mirrors the Ollama test for
// the Anthropic REST client. The bug that motivated v0.5b was
// specifically Ollama (qwen 32b), but the hardcoded 5-minute
// timeout was present in all three providers. Fixing one without
// fixing the others would leave the same landmine for future users.
func TestNewClaudeAppliesDefaultTimeout(t *testing.T) {
	p := NewClaude("claude-sonnet-4-5", 8192, 0.3, 0)
	if p.http.Timeout != DefaultHTTPTimeout {
		t.Errorf("Timeout = %s, want DefaultHTTPTimeout (%s)", p.http.Timeout, DefaultHTTPTimeout)
	}
}

func TestNewClaudeRespectsExplicitTimeout(t *testing.T) {
	want := 10 * time.Minute
	p := NewClaude("claude-sonnet-4-5", 8192, 0.3, want)
	if p.http.Timeout != want {
		t.Errorf("Timeout = %s, want %s", p.http.Timeout, want)
	}
}

func TestNewClaudeCLIAppliesDefaultTimeout(t *testing.T) {
	p := NewClaudeCLI("", 4096, 0.2, 0)
	if p.timeout != DefaultHTTPTimeout {
		t.Errorf("timeout = %s, want DefaultHTTPTimeout (%s)", p.timeout, DefaultHTTPTimeout)
	}
}

func TestNewClaudeCLIRespectsExplicitTimeout(t *testing.T) {
	want := 15 * time.Minute
	p := NewClaudeCLI("", 4096, 0.2, want)
	if p.timeout != want {
		t.Errorf("timeout = %s, want %s", p.timeout, want)
	}
}

// TestDefaultHTTPTimeoutIsReasonable is a guard against someone
// lowering DefaultHTTPTimeout back to 5 minutes "to save
// resources". 5 minutes was the v0.5 default and it caused the
// qwen 32b on M1 Pro failure. The floor is 15 minutes.
func TestDefaultHTTPTimeoutIsReasonable(t *testing.T) {
	if DefaultHTTPTimeout < 15*time.Minute {
		t.Errorf("DefaultHTTPTimeout = %s, must be >= 15m to cover 32b local models on M1 Pro-class hardware", DefaultHTTPTimeout)
	}
	// And a ceiling: a 3-hour default would be "I forgot this
	// existed" rather than intentional. Surface as a test.
	if DefaultHTTPTimeout > 60*time.Minute {
		t.Errorf("DefaultHTTPTimeout = %s, must be <= 60m to catch silent daemon hangs", DefaultHTTPTimeout)
	}
}
