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

// TestToOllamaMessagesTranslatesSimpleConversation verifies the
// baseline translation: system + user text + assistant text rounds
// trip to the flat Ollama message list without losing any fields.
func TestToOllamaMessagesTranslatesSimpleConversation(t *testing.T) {
	in := []ToolMessage{
		{
			Role: "user",
			Content: []ToolContentBlock{
				{Type: "text", Text: "find the bug"},
			},
		},
		{
			Role: "assistant",
			Content: []ToolContentBlock{
				{Type: "text", Text: "here it is"},
			},
		},
	}
	got, err := toOllamaMessages("You are a dev.", in)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d messages, want 3 (system+user+assistant)", len(got))
	}
	if got[0].Role != "system" || got[0].Content != "You are a dev." {
		t.Errorf("system message = %+v", got[0])
	}
	if got[1].Role != "user" || got[1].Content != "find the bug" {
		t.Errorf("user message = %+v", got[1])
	}
	if got[2].Role != "assistant" || got[2].Content != "here it is" {
		t.Errorf("assistant message = %+v", got[2])
	}
}

// TestToOllamaMessagesFansOutToolResults verifies that a user turn
// containing multiple tool_result blocks becomes multiple
// role="tool" messages on the Ollama side — each tool_result in
// the user ToolMessage becomes its own {role:"tool", content}
// message, because Ollama matches tool responses positionally.
func TestToOllamaMessagesFansOutToolResults(t *testing.T) {
	in := []ToolMessage{
		{
			Role: "user",
			Content: []ToolContentBlock{
				{Type: "text", Text: "consolidate the CTA"},
			},
		},
		{
			Role: "assistant",
			Content: []ToolContentBlock{
				{
					Type:      "tool_use",
					ToolUseID: "ollama_0",
					ToolName:  "read_file",
					ToolInput: json.RawMessage(`{"path":"a.json"}`),
				},
				{
					Type:      "tool_use",
					ToolUseID: "ollama_1",
					ToolName:  "read_file",
					ToolInput: json.RawMessage(`{"path":"b.json"}`),
				},
			},
		},
		{
			Role: "user",
			Content: []ToolContentBlock{
				{
					Type:              "tool_result",
					ToolUseID:         "ollama_0",
					ToolResultContent: `{"contactSales":"Contact Sales"}`,
				},
				{
					Type:              "tool_result",
					ToolUseID:         "ollama_1",
					ToolResultContent: `{"contactSales":"Contacter les ventes"}`,
				},
			},
		},
	}

	got, err := toOllamaMessages("", in)
	if err != nil {
		t.Fatal(err)
	}
	// Expected: user(1) + assistant(1 with 2 tool_calls) + tool(1) + tool(1) = 4 messages.
	if len(got) != 4 {
		t.Fatalf("got %d messages, want 4; got=%+v", len(got), got)
	}

	if got[0].Role != "user" || got[0].Content != "consolidate the CTA" {
		t.Errorf("msg[0] = %+v", got[0])
	}
	if got[1].Role != "assistant" {
		t.Errorf("msg[1].Role = %q, want assistant", got[1].Role)
	}
	if len(got[1].ToolCalls) != 2 {
		t.Errorf("assistant should carry 2 tool_calls, got %d", len(got[1].ToolCalls))
	}
	if got[1].ToolCalls[0].Function.Name != "read_file" {
		t.Errorf("tool_call[0] name = %q", got[1].ToolCalls[0].Function.Name)
	}
	if got[2].Role != "tool" || !strings.Contains(got[2].Content, "Contact Sales") {
		t.Errorf("msg[2] = %+v", got[2])
	}
	if got[3].Role != "tool" || !strings.Contains(got[3].Content, "Contacter les ventes") {
		t.Errorf("msg[3] = %+v", got[3])
	}
}

// TestToOllamaMessagesMarksErroredToolResults verifies that tool
// results with IsError=true get an "error: " prefix in the
// content field — Ollama's wire format has no IsError flag so we
// encode the signal into the text.
func TestToOllamaMessagesMarksErroredToolResults(t *testing.T) {
	in := []ToolMessage{
		{
			Role: "user",
			Content: []ToolContentBlock{
				{
					Type:              "tool_result",
					ToolUseID:         "x",
					ToolResultContent: "file not found: foo.go",
					ToolResultIsError: true,
				},
			},
		},
	}
	got, err := toOllamaMessages("", in)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Role != "tool" {
		t.Fatalf("got %+v", got)
	}
	if !strings.HasPrefix(got[0].Content, "error:") {
		t.Errorf("error flag should be encoded in content, got: %q", got[0].Content)
	}
}

// TestFromOllamaResponseEndTurn is the terminal-response case: no
// tool_calls, clean text output, StopReason should be "end_turn".
func TestFromOllamaResponseEndTurn(t *testing.T) {
	resp := &ollamaToolResp{
		Model:      "qwen2.5-coder:14b",
		DoneReason: "stop",
	}
	resp.Message.Role = "assistant"
	resp.Message.Content = "diff --git a/x b/x\n..."
	resp.PromptEvalCount = 42
	resp.EvalCount = 100

	got := fromOllamaResponse(resp)
	if got.StopReason != "end_turn" {
		t.Errorf("StopReason = %q, want end_turn", got.StopReason)
	}
	if !strings.HasPrefix(got.Content, "diff --git") {
		t.Errorf("Content = %q", got.Content)
	}
	if len(got.ToolUses) != 0 {
		t.Errorf("end_turn response should have no tool uses")
	}
	if got.Usage.InputTokens != 42 || got.Usage.OutputTokens != 100 {
		t.Errorf("usage = %+v", got.Usage)
	}
}

// TestFromOllamaResponseExtractsToolCallsAndSynthesizesIDs verifies
// the tool-use response path: Ollama's tool_calls (which have no
// IDs) get turned into internal ToolUse entries with synthesized
// "ollama_<idx>" IDs so the caller's correlation model still works.
func TestFromOllamaResponseExtractsToolCallsAndSynthesizesIDs(t *testing.T) {
	resp := &ollamaToolResp{
		Model: "qwen2.5-coder:14b",
	}
	resp.Message.Role = "assistant"
	resp.Message.Content = ""
	resp.Message.ToolCalls = []ollamaToolCall{
		{
			Function: ollamaToolCallFunction{
				Name:      "glob",
				Arguments: json.RawMessage(`{"pattern":"apps/marketing/**/*.tsx"}`),
			},
		},
		{
			Function: ollamaToolCallFunction{
				Name:      "read_file",
				Arguments: json.RawMessage(`{"path":"README.md"}`),
			},
		},
	}

	got := fromOllamaResponse(resp)
	if got.StopReason != "tool_use" {
		t.Errorf("StopReason = %q, want tool_use", got.StopReason)
	}
	if len(got.ToolUses) != 2 {
		t.Fatalf("got %d tool uses, want 2", len(got.ToolUses))
	}
	if got.ToolUses[0].ID != "ollama_0" || got.ToolUses[0].Name != "glob" {
		t.Errorf("tool[0] = %+v", got.ToolUses[0])
	}
	if got.ToolUses[1].ID != "ollama_1" || got.ToolUses[1].Name != "read_file" {
		t.Errorf("tool[1] = %+v", got.ToolUses[1])
	}
	// AssistantMessage must round-trip the tool_use blocks so the
	// caller can append it to history for the next turn.
	if got.AssistantMessage.Role != "assistant" {
		t.Errorf("AssistantMessage.Role = %q", got.AssistantMessage.Role)
	}
	toolUseBlocks := 0
	for _, b := range got.AssistantMessage.Content {
		if b.Type == "tool_use" {
			toolUseBlocks++
		}
	}
	if toolUseBlocks != 2 {
		t.Errorf("AssistantMessage should carry 2 tool_use blocks, got %d", toolUseBlocks)
	}
}

// TestFromOllamaResponseMaxTokens is the "model hit the output
// token cap" case. Ollama encodes this as done_reason=="length";
// we translate to StopReason=="max_tokens" so the Implementer's
// loop treats it the same way the Anthropic path does.
func TestFromOllamaResponseMaxTokens(t *testing.T) {
	resp := &ollamaToolResp{DoneReason: "length"}
	resp.Message.Role = "assistant"
	resp.Message.Content = "partial output"
	got := fromOllamaResponse(resp)
	if got.StopReason != "max_tokens" {
		t.Errorf("StopReason = %q", got.StopReason)
	}
}

// TestToOllamaToolsShapeMatchesOpenAIStyle spot-checks the wire
// shape of the tools array: Ollama wants OpenAI's
// {type: "function", function: {name, description, parameters}}
// wrapper, not Anthropic's flat {name, description, input_schema}.
func TestToOllamaToolsShapeMatchesOpenAIStyle(t *testing.T) {
	defs := []ToolDefinition{
		{
			Name:        "read_file",
			Description: "Read a file",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`),
		},
	}
	got := toOllamaTools(defs)
	if len(got) != 1 {
		t.Fatalf("got %d tools, want 1", len(got))
	}
	if got[0].Type != "function" {
		t.Errorf("tool type = %q, want function", got[0].Type)
	}
	if got[0].Function.Name != "read_file" {
		t.Errorf("function name = %q", got[0].Function.Name)
	}
	// Marshal and verify the wire shape matches Ollama's expectation.
	data, err := json.Marshal(got[0])
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	for _, want := range []string{
		`"type":"function"`,
		`"function":{`,
		`"name":"read_file"`,
		`"description":"Read a file"`,
		`"parameters":{`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("wire shape missing %q; got:\n%s", want, s)
		}
	}
}

// TestOllamaCompleteWithToolsWiresAgainstFakeServer is the
// full-stack round-trip: stand up an httptest server that mimics
// the Ollama /api/chat endpoint, point an *Ollama at it, fire a
// tool-aware request, and verify both the outgoing request body
// AND the incoming response parsing.
func TestOllamaCompleteWithToolsWiresAgainstFakeServer(t *testing.T) {
	var gotBody map[string]any
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"model": "qwen2.5-coder:14b",
			"message": {
				"role": "assistant",
				"content": "",
				"tool_calls": [
					{"function": {"name": "read_file", "arguments": {"path": "foo.go"}}}
				]
			},
			"done": true,
			"done_reason": "stop",
			"prompt_eval_count": 15,
			"eval_count": 42
		}`))
	}))
	defer fake.Close()

	o := &Ollama{
		endpoint:    fake.URL,
		model:       "qwen2.5-coder:14b",
		maxTokens:   2048,
		temperature: 0.2,
		http:        fake.Client(),
	}

	req := ToolAwareRequest{
		System: "You are an implementer.",
		Tools: []ToolDefinition{
			{
				Name:        "read_file",
				Description: "Read a file",
				InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
			},
		},
		Messages: []ToolMessage{
			{
				Role:    "user",
				Content: []ToolContentBlock{{Type: "text", Text: "Fix foo.go"}},
			},
		},
	}

	resp, err := o.CompleteWithTools(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}

	// Response side: one tool_use block extracted, stop_reason tool_use.
	if resp.StopReason != "tool_use" {
		t.Errorf("StopReason = %q, want tool_use", resp.StopReason)
	}
	if len(resp.ToolUses) != 1 || resp.ToolUses[0].Name != "read_file" {
		t.Errorf("tool uses = %+v", resp.ToolUses)
	}

	// Request side: the body the fake server saw must have the
	// right Ollama shape.
	tools, ok := gotBody["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("request tools = %+v", gotBody["tools"])
	}
	toolObj := tools[0].(map[string]any)
	if toolObj["type"] != "function" {
		t.Errorf("tool.type = %v", toolObj["type"])
	}

	msgs, ok := gotBody["messages"].([]any)
	if !ok || len(msgs) != 2 {
		// expected: system + user
		t.Fatalf("request messages = %+v", gotBody["messages"])
	}
	if msgs[0].(map[string]any)["role"] != "system" {
		t.Errorf("msg[0] role = %v", msgs[0])
	}
	if msgs[1].(map[string]any)["role"] != "user" {
		t.Errorf("msg[1] role = %v", msgs[1])
	}
}

// TestOllamaCompleteWithToolsSurfacesDaemonError verifies that
// when Ollama returns an error envelope (typical for models that
// don't support tool use — the daemon rejects the request), we
// propagate it as a Go error with a hint about compatible models.
func TestOllamaCompleteWithToolsSurfacesDaemonError(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"error": "registry.ollama.ai/library/outdated-model does not support tools"}`))
	}))
	defer fake.Close()

	o := &Ollama{
		endpoint: fake.URL,
		model:    "outdated-model",
		http:     fake.Client(),
	}
	_, err := o.CompleteWithTools(context.Background(), ToolAwareRequest{
		Messages: []ToolMessage{{
			Role:    "user",
			Content: []ToolContentBlock{{Type: "text", Text: "hi"}},
		}},
	})
	if err == nil {
		t.Fatal("expected error on non-tool-capable model")
	}
	if !strings.Contains(err.Error(), "does not support tools") {
		t.Errorf("error should mention lack of tool support; got: %v", err)
	}
	if !strings.Contains(err.Error(), "qwen2.5-coder") {
		t.Errorf("error should suggest compatible models; got: %v", err)
	}
}

// TestOllamaImplementsToolAwareProvider is a compile-time guard.
// If someone removes or renames CompleteWithTools on *Ollama, this
// fails at build time.
func TestOllamaImplementsToolAwareProvider(t *testing.T) {
	var _ ToolAwareProvider = (*Ollama)(nil)
}
