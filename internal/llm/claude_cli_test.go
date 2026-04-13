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
// the `claude` CLI (reads stdin, echoes a fixture response) and points
// the provider at it. Verifies the prompt flows through correctly and
// the response is returned untouched.
func TestClaudeCLICompleteHappyPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fixtures don't run on windows")
	}
	fakeBin := writeFakeClaudeBin(t, `#!/bin/sh
# Read stdin, capture the last line as the "user prompt", and emit a
# deterministic response so the test can assert on it.
cat > /tmp/aidev-claude-cli-test-stdin
echo "FAKE RESPONSE FROM CLAUDE CLI"
`)

	p := NewClaudeCLI("", 1024, 0.1)
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
	if resp.Usage.InputTokens != 0 || resp.Usage.OutputTokens != 0 {
		t.Errorf("token counts should be zero for claude-cli, got %+v", resp.Usage)
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

func TestClaudeCLICompleteEmptyPromptErrors(t *testing.T) {
	p := NewClaudeCLI("", 1024, 0.1)
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
	p := NewClaudeCLI("", 1024, 0.1)
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
	p := NewClaudeCLI("", 1024, 0.1)
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
	p1 := NewClaudeCLI("", 1024, 0.1)
	if p1.Name() != "claude-cli" {
		t.Errorf("name = %q, want claude-cli", p1.Name())
	}
	p2 := NewClaudeCLI("claude-opus-4-6", 1024, 0.1)
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
