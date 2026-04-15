package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// extractEmbeddedToolCalls — content-embedded tool call salvage
// ---------------------------------------------------------------------------
//
// The regression we're guarding against: tool-capable Ollama
// models (qwen2.5-coder:14b specifically) sometimes emit tool
// calls as JSON in the message.content field instead of
// populating the structured tool_calls field. When that
// happens, fromOllamaResponse needs to salvage the intent so
// the pipeline doesn't hard-fail on "content doesn't start
// with diff --git".
//
// We accept three wire shapes in priority order:
//   {"name": "...", "arguments": {...}}            (Qwen, LLaMA)
//   {"function": "...", "parameters": {...}}      (OpenAI legacy)
//   {"tool": "...", "input": {...}}                (Anthropic-ish)

func TestExtractEmbeddedToolCallsNameArguments(t *testing.T) {
	in := `{"name": "read_file", "arguments": {"path": "apps/marketing/src/foo.tsx"}}`
	got := extractEmbeddedToolCalls(in)
	if len(got) != 1 {
		t.Fatalf("got %d calls, want 1", len(got))
	}
	if got[0].Name != "read_file" {
		t.Errorf("Name = %q, want read_file", got[0].Name)
	}
	if !strings.Contains(string(got[0].Arguments), "apps/marketing") {
		t.Errorf("Arguments lost the path; got %q", got[0].Arguments)
	}
}

func TestExtractEmbeddedToolCallsFunctionParameters(t *testing.T) {
	in := `{"function": "glob", "parameters": {"pattern": "**/*.tsx"}}`
	got := extractEmbeddedToolCalls(in)
	if len(got) != 1 || got[0].Name != "glob" {
		t.Errorf("got %+v, want one call with name=glob", got)
	}
}

func TestExtractEmbeddedToolCallsToolInput(t *testing.T) {
	in := `{"tool": "grep", "input": {"pattern": "contactSales"}}`
	got := extractEmbeddedToolCalls(in)
	if len(got) != 1 || got[0].Name != "grep" {
		t.Errorf("got %+v, want one call with name=grep", got)
	}
}

func TestExtractEmbeddedToolCallsArrayOfCalls(t *testing.T) {
	// Some models emit multiple tool calls as a JSON array in one response.
	in := `[
		{"name": "read_file", "arguments": {"path": "a.go"}},
		{"name": "read_file", "arguments": {"path": "b.go"}}
	]`
	got := extractEmbeddedToolCalls(in)
	if len(got) != 2 {
		t.Fatalf("got %d calls, want 2", len(got))
	}
	if got[0].Name != "read_file" || got[1].Name != "read_file" {
		t.Errorf("names wrong: %+v", got)
	}
}

func TestExtractEmbeddedToolCallsStripsCodeFence(t *testing.T) {
	in := "```json\n" + `{"name": "list_dir", "arguments": {"path": "."}}` + "\n```"
	got := extractEmbeddedToolCalls(in)
	if len(got) != 1 || got[0].Name != "list_dir" {
		t.Errorf("fenced JSON should parse; got %+v", got)
	}
}

func TestExtractEmbeddedToolCallsRejectsProseWithJSON(t *testing.T) {
	// Conservative: if the content has prose mixed with JSON,
	// we do NOT salvage it — that's a different failure mode
	// (model is confused about output format) and format
	// correction is the right path, not silent salvage.
	in := `Let me read the file. {"name": "read_file", "arguments": {"path": "foo.go"}}`
	got := extractEmbeddedToolCalls(in)
	if len(got) != 0 {
		t.Errorf("prose + JSON should not salvage, got %+v", got)
	}
}

func TestExtractEmbeddedToolCallsRejectsNonToolJSON(t *testing.T) {
	// A JSON object that isn't a tool call — e.g. the model
	// emits a config blob or a diff metadata wrapper — should
	// NOT be salvaged. extractEmbeddedToolCalls only returns
	// calls when it recognizes one of the three accepted shapes.
	in := `{"status": "ok", "result": [1,2,3]}`
	got := extractEmbeddedToolCalls(in)
	if len(got) != 0 {
		t.Errorf("non-tool-call JSON should return nothing; got %+v", got)
	}
}

func TestExtractEmbeddedToolCallsRejectsEmpty(t *testing.T) {
	if got := extractEmbeddedToolCalls(""); len(got) != 0 {
		t.Errorf("empty input should return nothing; got %+v", got)
	}
	if got := extractEmbeddedToolCalls("   \n\n   "); len(got) != 0 {
		t.Errorf("whitespace input should return nothing; got %+v", got)
	}
}

// ---------------------------------------------------------------------------
// fromOllamaResponse with content-embedded salvage
// ---------------------------------------------------------------------------

func TestFromOllamaResponseSalvagesContentEmbeddedToolCall(t *testing.T) {
	// This is the exact scenario that v0.5 hit on the user's
	// smoke test: qwen2.5-coder:14b emits a tool call as JSON
	// in content instead of populating tool_calls. Without
	// salvage, stop_reason becomes end_turn and the content
	// goes to the diff validator which rejects it.
	resp := &ollamaToolResp{
		Model: "qwen2.5-coder:14b",
	}
	resp.Message.Role = "assistant"
	resp.Message.Content = `{"name": "read_file", "arguments": {"path": "apps/marketing/src/foo.tsx"}}`
	resp.DoneReason = "stop"

	got := fromOllamaResponse(resp)

	if got.StopReason != "tool_use" {
		t.Errorf("StopReason = %q, want tool_use (should have salvaged the embedded call)", got.StopReason)
	}
	if len(got.ToolUses) != 1 {
		t.Fatalf("got %d tool uses, want 1", len(got.ToolUses))
	}
	if got.ToolUses[0].Name != "read_file" {
		t.Errorf("tool name = %q", got.ToolUses[0].Name)
	}
	// Content must be cleared so downstream validators don't
	// see the raw JSON as a diff attempt.
	if got.Content != "" {
		t.Errorf("Content should be cleared after salvage, got %q", got.Content)
	}
	// The AssistantMessage must carry the tool_use block (not
	// a text block with the raw JSON) so the next turn's
	// history is clean.
	var sawToolUse, sawText bool
	for _, b := range got.AssistantMessage.Content {
		if b.Type == "tool_use" {
			sawToolUse = true
		}
		if b.Type == "text" {
			sawText = true
		}
	}
	if !sawToolUse {
		t.Errorf("AssistantMessage should contain a tool_use block")
	}
	if sawText {
		t.Errorf("AssistantMessage should NOT contain the raw JSON as a text block")
	}
}

func TestFromOllamaResponseMixesStructuredAndEmbeddedCalls(t *testing.T) {
	// Unlikely real-world case but worth covering: a response
	// that has both a structured tool_call AND a content-embedded
	// one should end up with both tool uses, with unique
	// correlation IDs.
	resp := &ollamaToolResp{
		Model: "qwen2.5-coder:14b",
	}
	resp.Message.Role = "assistant"
	resp.Message.Content = `{"name": "grep", "arguments": {"pattern": "foo"}}`
	resp.Message.ToolCalls = []ollamaToolCall{
		{
			Function: ollamaToolCallFunction{
				Name:      "read_file",
				Arguments: json.RawMessage(`{"path": "x.go"}`),
			},
		},
	}

	got := fromOllamaResponse(resp)

	if got.StopReason != "tool_use" {
		t.Errorf("StopReason = %q", got.StopReason)
	}
	if len(got.ToolUses) != 2 {
		t.Fatalf("got %d tool uses, want 2", len(got.ToolUses))
	}
	// Structured call first, embedded call second.
	if got.ToolUses[0].Name != "read_file" {
		t.Errorf("tool[0] = %q, want read_file (structured)", got.ToolUses[0].Name)
	}
	if got.ToolUses[1].Name != "grep" {
		t.Errorf("tool[1] = %q, want grep (embedded)", got.ToolUses[1].Name)
	}
	if got.ToolUses[0].ID == got.ToolUses[1].ID {
		t.Error("tool use IDs should be unique")
	}
}

func TestFromOllamaResponsePlainContentPassesThrough(t *testing.T) {
	// Sanity check: content that isn't a tool call (a real
	// diff, prose, anything) passes through unchanged and
	// stop_reason stays end_turn.
	resp := &ollamaToolResp{
		Model: "qwen2.5-coder:14b",
	}
	resp.Message.Role = "assistant"
	resp.Message.Content = "diff --git a/x.go b/x.go\n--- a/x.go\n+++ b/x.go"
	resp.DoneReason = "stop"

	got := fromOllamaResponse(resp)
	if got.StopReason != "end_turn" {
		t.Errorf("StopReason = %q, want end_turn", got.StopReason)
	}
	if !strings.HasPrefix(got.Content, "diff --git") {
		t.Errorf("Content should pass through; got %q", got.Content)
	}
	if len(got.ToolUses) != 0 {
		t.Errorf("plain diff should not produce tool uses; got %+v", got.ToolUses)
	}
}

// ---------------------------------------------------------------------------
// trimCodeFence helper — used by extractEmbeddedToolCalls
// ---------------------------------------------------------------------------

func TestTrimCodeFenceStripsMarkdownWrap(t *testing.T) {
	in := "```json\n{\"name\": \"read_file\"}\n```"
	got := trimCodeFence(in)
	if got != `{"name": "read_file"}` {
		t.Errorf("got %q", got)
	}
}

func TestTrimCodeFenceLeavesBareJSONAlone(t *testing.T) {
	in := `{"name": "read_file"}`
	got := trimCodeFence(in)
	if got != in {
		t.Errorf("got %q, want %q", got, in)
	}
}

func TestTrimCodeFenceStripsPartialFences(t *testing.T) {
	// Missing closing fence — still strip the opening one.
	in := "```\n{\"name\": \"x\"}"
	got := trimCodeFence(in)
	if got != `{"name": "x"}` {
		t.Errorf("got %q", got)
	}
}
