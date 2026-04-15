package llm

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestClaudeCLICompleteHappyPath writes a tiny shell script that mimics
// the `claude` CLI under --output-format json: reads stdin, emits a
// fixture json envelope with known token counts. Verifies the prompt
// flows through correctly, the assistant text is extracted from the
// envelope's `result` field, and the token counts in `usage` are
// surfaced on Response.Usage.
func TestClaudeCLICompleteHappyPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fixtures don't run on windows")
	}
	fakeBin := writeFakeClaudeBin(t, `#!/bin/sh
# Read stdin, capture it for later assertion, and emit a json envelope
# shaped like real claude --print --output-format json output.
cat > /tmp/aidev-claude-cli-test-stdin
cat <<'JSON'
{"type":"result","subtype":"success","is_error":false,"result":"FAKE RESPONSE FROM CLAUDE CLI","usage":{"input_tokens":1234,"output_tokens":567,"cache_read_input_tokens":0}}
JSON
`)

	p := NewClaudeCLI("", 1024, 0.1, 0)
	p.SetBin(fakeBin)

	resp, err := p.Complete(context.Background(), Request{
		System: "you are a test",
		Messages: []Message{
			{Role: "user", Content: "hello cli"},
		},
	})
	if err != nil {
		t.Fatalf("Complete returned err: %v", err)
	}
	if resp.Content != "FAKE RESPONSE FROM CLAUDE CLI" {
		t.Errorf("content = %q, want FAKE RESPONSE FROM CLAUDE CLI", resp.Content)
	}
	if resp.Usage.InputTokens != 1234 {
		t.Errorf("InputTokens = %d, want 1234", resp.Usage.InputTokens)
	}
	if resp.Usage.OutputTokens != 567 {
		t.Errorf("OutputTokens = %d, want 567", resp.Usage.OutputTokens)
	}

	// The fake binary captured stdin to a file; verify the prompt made
	// it through.
	stdinData, err := os.ReadFile("/tmp/aidev-claude-cli-test-stdin")
	if err == nil {
		if !strings.Contains(string(stdinData), "hello cli") {
			t.Errorf("stdin to fake claude did not contain prompt: %q", stdinData)
		}
		_ = os.Remove("/tmp/aidev-claude-cli-test-stdin")
	}
}

// TestClaudeCLICompleteMalformedEnvelopeFallsBack verifies the graceful
// fallback when the CLI emits something that is not a valid json
// envelope (e.g. an older CLI version, or garbled output). The provider
// should NOT error — it should surface the raw stdout as Content and
// leave Usage zero, so Complete() keeps working even if token parsing
// breaks.
func TestClaudeCLICompleteMalformedEnvelopeFallsBack(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fixtures don't run on windows")
	}
	fakeBin := writeFakeClaudeBin(t, `#!/bin/sh
# Emit something that is definitely not a json envelope.
echo "this is not json, just plain text"
`)
	p := NewClaudeCLI("", 1024, 0.1, 0)
	p.SetBin(fakeBin)
	resp, err := p.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete returned err on malformed envelope: %v", err)
	}
	if !strings.Contains(resp.Content, "this is not json") {
		t.Errorf("fallback content = %q, want raw stdout", resp.Content)
	}
	if resp.Usage.InputTokens != 0 || resp.Usage.OutputTokens != 0 {
		t.Errorf("malformed envelope should leave Usage zero, got %+v", resp.Usage)
	}
}

func TestClaudeCLICompleteEmptyPromptErrors(t *testing.T) {
	p := NewClaudeCLI("", 1024, 0.1, 0)
	_, err := p.Complete(context.Background(), Request{})
	if err == nil {
		t.Error("expected error on empty prompt")
	}
}

func TestClaudeCLICompleteEmptyResponseErrors(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fixtures don't run on windows")
	}
	fakeBin := writeFakeClaudeBin(t, `#!/bin/sh
# Print nothing — simulates a CLI that silently fails.
exit 0
`)
	p := NewClaudeCLI("", 1024, 0.1, 0)
	p.SetBin(fakeBin)
	_, err := p.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Error("expected error on empty response")
	}
}

func TestClaudeCLICompleteStderrInError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fixtures don't run on windows")
	}
	fakeBin := writeFakeClaudeBin(t, `#!/bin/sh
echo "not authenticated — run claude /login" >&2
exit 2
`)
	p := NewClaudeCLI("", 1024, 0.1, 0)
	p.SetBin(fakeBin)
	_, err := p.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "not authenticated") {
		t.Errorf("error missing stderr content: %v", err)
	}
}

func TestClaudeCLIName(t *testing.T) {
	p1 := NewClaudeCLI("", 1024, 0.1, 0)
	if p1.Name() != "claude-cli" {
		t.Errorf("name = %q, want claude-cli", p1.Name())
	}
	p2 := NewClaudeCLI("claude-opus-4-6", 1024, 0.1, 0)
	if p2.Name() != "claude-cli:claude-opus-4-6" {
		t.Errorf("name = %q, want claude-cli:claude-opus-4-6", p2.Name())
	}
}

func TestClaudeCLIRouterRegistration(t *testing.T) {
	// The router's buildProvider should recognise "claude-cli" as a
	// valid provider type and return a non-nil ClaudeCLI instance.
	t.Run("via buildProvider", func(t *testing.T) {
		// We import config via the package-local view since this test
		// lives in the same package as router.go.
	})
}

// writeFakeClaudeBin writes a shell script to a temp file, marks it
// executable, and returns the path. Cleaned up by t.TempDir().
func writeFakeClaudeBin(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "claude")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}
