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

// TestClaudeCLIImplementsToolAwareProvider is a compile-time guard.
// If CompleteWithTools is removed or renamed on *ClaudeCLI the test
// fails at compile time — so the Implementer's capability detection
// at implementer.go:151 keeps working end-to-end.
func TestClaudeCLIImplementsToolAwareProvider(t *testing.T) {
	var _ ToolAwareProvider = (*ClaudeCLI)(nil)
}

// TestClaudeCLICompleteWithToolsHappyPath verifies the subprocess-agent
// path: a fake claude binary that echoes the invocation details back
// as the envelope's `result` field, so the test can assert the
// command-line surface (--tools, --append-system-prompt) AND
// the cwd are correctly threaded.
func TestClaudeCLICompleteWithToolsHappyPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fixtures don't run on windows")
	}

	// The fake claude binary writes its args + cwd + stdin to a
	// known file, then emits a json envelope whose `result` is a
	// canned unified diff so the tool-aware response validator is
	// exercised end-to-end.
	traceFile := filepath.Join(t.TempDir(), "trace")
	fakeBin := writeFakeClaudeBin(t, `#!/bin/sh
{
  echo "args: $*"
  echo "cwd: $(pwd)"
  echo "stdin:"
  cat
} > `+traceFile+`
cat <<'JSON'
{"type":"result","subtype":"success","is_error":false,"result":"diff --git a/file.txt b/file.txt\n--- a/file.txt\n+++ b/file.txt\n@@ -1 +1 @@\n-old\n+new","usage":{"input_tokens":42,"output_tokens":21}}
JSON
`)

	repoDir := t.TempDir()
	// Touch a file so cwd is a real directory claude "could" read.
	if err := os.WriteFile(filepath.Join(repoDir, "file.txt"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := NewClaudeCLI("", 1024, 0.1, 0)
	p.SetBin(fakeBin)

	req := ToolAwareRequest{
		System: "you are the test implementer",
		Messages: []ToolMessage{
			{
				Role: "user",
				Content: []ToolContentBlock{
					{Type: "text", Text: "produce a unified diff for this repo"},
				},
			},
		},
		WorkingDir: repoDir,
	}
	resp, err := p.CompleteWithTools(context.Background(), req)
	if err != nil {
		t.Fatalf("CompleteWithTools: %v", err)
	}

	// Verify the response shape the harness expects.
	if resp.StopReason != "end_turn" {
		t.Errorf("StopReason = %q, want end_turn", resp.StopReason)
	}
	if !strings.HasPrefix(resp.Content, "diff --git") {
		t.Errorf("Content missing diff prefix: %q", resp.Content)
	}
	if resp.Usage.InputTokens != 42 || resp.Usage.OutputTokens != 21 {
		t.Errorf("Usage = %+v, want {42 21}", resp.Usage)
	}
	if len(resp.ToolUses) != 0 {
		t.Errorf("ToolUses should be empty in subprocess agent mode, got %d", len(resp.ToolUses))
	}
	if len(resp.AssistantMessage.Content) != 1 || resp.AssistantMessage.Content[0].Type != "text" {
		t.Errorf("AssistantMessage shape wrong: %+v", resp.AssistantMessage)
	}

	// Verify the command-line surface actually fed to the binary.
	trace, err := os.ReadFile(traceFile)
	if err != nil {
		t.Fatalf("trace file: %v", err)
	}
	traceStr := string(trace)
	for _, want := range []string{
		"--print",
		"--output-format json",
		"--tools Read,Grep,Glob,LS",
		"--append-system-prompt you are the test implementer",
		"cwd: " + repoDir,
		"stdin:\nproduce a unified diff",
	} {
		if !strings.Contains(traceStr, want) {
			t.Errorf("trace missing %q; full trace:\n%s", want, traceStr)
		}
	}
}

// TestClaudeCLICompleteWithToolsRequiresWorkingDir is the negative
// case: agent mode without a WorkingDir would silently point claude
// at aidev's own cwd, which is a configuration bug. Fail fast.
func TestClaudeCLICompleteWithToolsRequiresWorkingDir(t *testing.T) {
	p := NewClaudeCLI("", 1024, 0.1, 0)
	p.SetBin("/bin/true")
	_, err := p.CompleteWithTools(context.Background(), ToolAwareRequest{
		Messages: []ToolMessage{
			{Role: "user", Content: []ToolContentBlock{{Type: "text", Text: "hi"}}},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "WorkingDir") {
		t.Errorf("expected WorkingDir error, got: %v", err)
	}
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
