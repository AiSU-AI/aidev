package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestToClaudeMessagesRoundTrip verifies every supported content
// block type survives translation to the Anthropic wire shape with
// the right field names. Regression guard against a refactor that
// accidentally drops one of tool_use, tool_result, or text.
func TestToClaudeMessagesRoundTrip(t *testing.T) {
	in := []ToolMessage{
		{
			Role: "user",
			Content: []ToolContentBlock{
				{Type: "text", Text: "Find call sites"},
			},
		},
		{
			Role: "assistant",
			Content: []ToolContentBlock{
				{Type: "text", Text: "I'll grep for them."},
				{
					Type:      "tool_use",
					ToolUseID: "toolu_abc123",
					ToolName:  "grep",
					ToolInput: json.RawMessage(`{"pattern":"contactSales","paths":["apps/marketing/src"]}`),
				},
			},
		},
		{
			Role: "user",
			Content: []ToolContentBlock{
				{
					Type:              "tool_result",
					ToolUseID:         "toolu_abc123",
					ToolResultContent: "pricing.tsx:56: t(\"contactSales\")",
					ToolResultIsError: false,
				},
			},
		},
	}

	out, err := toClaudeMessages(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 {
		t.Fatalf("got %d messages, want 3", len(out))
	}

	// Marshal to JSON and spot-check the wire shape matches what
	// Anthropic's API expects. This is the real contract — if the
	// field names drift, the API rejects the request.
	data, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	for _, want := range []string{
		`"type":"text"`,
		`"text":"Find call sites"`,
		`"type":"tool_use"`,
		`"id":"toolu_abc123"`,
		`"name":"grep"`,
		`"input":{"pattern":"contactSales"`,
		`"type":"tool_result"`,
		`"tool_use_id":"toolu_abc123"`,
		`"content":"pricing.tsx:56: t(\"contactSales\")"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("marshaled wire shape missing %q\nfull output:\n%s", want, s)
		}
	}
}

func TestToClaudeMessagesRejectsUnknownBlockType(t *testing.T) {
	_, err := toClaudeMessages([]ToolMessage{
		{
			Role: "user",
			Content: []ToolContentBlock{
				{Type: "image", Text: "???"},
			},
		},
	})
	if err == nil {
		t.Error("expected error for unknown block type")
	}
}

// TestFromClaudeResponseExtractsToolUses covers the response-side
// translation: an Anthropic tool_use response must yield a
// ToolAwareResponse with StopReason="tool_use", the correct
// ToolUses list, and an AssistantMessage the caller can round-trip.
func TestFromClaudeResponseExtractsToolUses(t *testing.T) {
	resp := &claudeToolsResp{
		StopReason: "tool_use",
		Model:      "claude-sonnet-4-5",
		Content: []claudeContentBlock{
			{Type: "text", Text: "Let me check the locale files."},
			{
				Type:  "tool_use",
				ID:    "toolu_01",
				Name:  "glob",
				Input: json.RawMessage(`{"pattern":"apps/marketing/src/lib/i18n/translations/*/common.json"}`),
			},
			{
				Type:  "tool_use",
				ID:    "toolu_02",
				Name:  "read_file",
				Input: json.RawMessage(`{"path":"apps/marketing/src/components/pages/pricing/pricing.tsx"}`),
			},
		},
	}
	resp.Usage.InputTokens = 1234
	resp.Usage.OutputTokens = 56

	got := fromClaudeResponse(resp)

	if got.StopReason != "tool_use" {
		t.Errorf("StopReason = %q, want tool_use", got.StopReason)
	}
	if got.Content != "Let me check the locale files." {
		t.Errorf("Content = %q", got.Content)
	}
	if len(got.ToolUses) != 2 {
		t.Fatalf("got %d tool uses, want 2", len(got.ToolUses))
	}
	if got.ToolUses[0].Name != "glob" || got.ToolUses[0].ID != "toolu_01" {
		t.Errorf("tool[0] = %+v", got.ToolUses[0])
	}
	if got.ToolUses[1].Name != "read_file" || got.ToolUses[1].ID != "toolu_02" {
		t.Errorf("tool[1] = %+v", got.ToolUses[1])
	}
	// Usage flows through.
	if got.Usage.InputTokens != 1234 || got.Usage.OutputTokens != 56 {
		t.Errorf("usage = %+v", got.Usage)
	}
	// AssistantMessage should contain every block from the response
	// so the caller can append it to its history on the next turn.
	if got.AssistantMessage.Role != "assistant" {
		t.Errorf("AssistantMessage role = %q", got.AssistantMessage.Role)
	}
	if len(got.AssistantMessage.Content) != 3 {
		t.Errorf("AssistantMessage content = %d blocks, want 3", len(got.AssistantMessage.Content))
	}
}

func TestFromClaudeResponseEndTurn(t *testing.T) {
	resp := &claudeToolsResp{
		StopReason: "end_turn",
		Content: []claudeContentBlock{
			{Type: "text", Text: "diff --git a/foo b/foo\n..."},
		},
	}
	got := fromClaudeResponse(resp)
	if got.StopReason != "end_turn" {
		t.Errorf("StopReason = %q", got.StopReason)
	}
	if !strings.HasPrefix(got.Content, "diff --git") {
		t.Errorf("Content = %q", got.Content)
	}
	if len(got.ToolUses) != 0 {
		t.Errorf("end_turn response should have no pending tool uses, got %d", len(got.ToolUses))
	}
}

// TestClaudeCompleteWithToolsWiresAgainstFakeServer spins up an
// httptest server that mimics the Anthropic Messages API tool-use
// response shape, points a Claude provider at it, and verifies the
// round-trip end-to-end. This is the closest unit test we can get
// to "did we break the actual wire format" without hitting the real
// API — any field rename or JSON structure change will fail here.
func TestClaudeCompleteWithToolsWiresAgainstFakeServer(t *testing.T) {
	var gotBody map[string]any
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"content": [
				{"type": "text", "text": "I need to see the file."},
				{"type": "tool_use", "id": "toolu_01", "name": "read_file", "input": {"path": "foo.go"}}
			],
			"model": "claude-sonnet-4-5",
			"stop_reason": "tool_use",
			"usage": {"input_tokens": 10, "output_tokens": 20}
		}`))
	}))
	defer fake.Close()

	c := &Claude{
		apiKey:      "test-key",
		model:       "claude-sonnet-4-5",
		maxTokens:   1024,
		temperature: 0.2,
		endpoint:    fake.URL,
		http:        fake.Client(),
	}

	req := ToolAwareRequest{
		System: "You are a test.",
		Tools: []ToolDefinition{
			{
				Name:        "read_file",
				Description: "Read a file",
				InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
			},
		},
		Messages: []ToolMessage{
			{
				Role: "user",
				Content: []ToolContentBlock{
					{Type: "text", Text: "Fix the bug in foo.go"},
				},
			},
		},
		MaxTokens: 0, // fall through to tier default
	}

	resp, err := c.CompleteWithTools(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}

	if resp.StopReason != "tool_use" {
		t.Errorf("stop reason = %q", resp.StopReason)
	}
	if len(resp.ToolUses) != 1 || resp.ToolUses[0].Name != "read_file" {
		t.Errorf("tool uses = %+v", resp.ToolUses)
	}

	// Verify the request body the fake server received has the
	// right wire shape: tools array, messages array with typed
	// content blocks.
	if _, ok := gotBody["tools"]; !ok {
		t.Error("request missing tools array")
	}
	msgs, ok := gotBody["messages"].([]any)
	if !ok || len(msgs) != 1 {
		t.Fatalf("request messages = %v", gotBody["messages"])
	}
	first, ok := msgs[0].(map[string]any)
	if !ok {
		t.Fatalf("message 0 = %v", msgs[0])
	}
	content, ok := first["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("content = %v", first["content"])
	}
	block, ok := content[0].(map[string]any)
	if !ok || block["type"] != "text" || block["text"] != "Fix the bug in foo.go" {
		t.Errorf("content block = %v", content[0])
	}
}

// TestClaudeCompleteWithToolsErrorResponse verifies the error path:
// when the API returns an error envelope, we propagate it as a Go
// error instead of a malformed response.
func TestClaudeCompleteWithToolsErrorResponse(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"error": {"type": "invalid_request_error", "message": "tools malformed"}}`))
	}))
	defer fake.Close()

	c := &Claude{
		apiKey:      "test-key",
		model:       "claude-sonnet-4-5",
		maxTokens:   1024,
		temperature: 0.2,
		endpoint:    fake.URL,
		http:        fake.Client(),
	}
	_, err := c.CompleteWithTools(context.Background(), ToolAwareRequest{
		Messages: []ToolMessage{{
			Role:    "user",
			Content: []ToolContentBlock{{Type: "text", Text: "hi"}},
		}},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "tools malformed") {
		t.Errorf("error message = %v", err)
	}
}

// TestClaudeImplementsToolAwareProvider is a compile-time check that
// the interface implementation is still valid. If someone removes or
// renames CompleteWithTools on Claude, this test fails at compile time.
func TestClaudeImplementsToolAwareProvider(t *testing.T) {
	var _ ToolAwareProvider = (*Claude)(nil)
}
