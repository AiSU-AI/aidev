package llm

// Ollama tool-use support (v0.4a) — ToolAwareProvider implementation
// for the local Ollama daemon.
//
// Ollama's tool-use wire format is OpenAI-style function calling,
// which is meaningfully different from Anthropic's content-block
// model that aidev's internal ToolContentBlock / ToolMessage types
// are shaped around:
//
//   Anthropic: assistant message with a content ARRAY of typed
//              blocks (text, tool_use); tool_result blocks live
//              inside a user message and correlate to a prior
//              tool_use by ID.
//
//   Ollama:    assistant message with a plain string content AND
//              a separate tool_calls field; tool results are
//              separate top-level messages with role="tool" and no
//              correlation ID (matched by position to the prior
//              assistant's tool_calls).
//
// The translation layer below converts between the two. Ollama
// tool_calls don't carry IDs, so we synthesize deterministic
// "ollama_<index>" IDs when parsing responses so the caller can
// keep using the same internal correlation model the Anthropic
// path uses. On the way back out, those synthetic IDs are dropped
// because Ollama's wire format doesn't need them.
//
// Supported models (as of v0.4a): qwen2.5, qwen2.5-coder, llama3.1,
// llama3.2, mistral-nemo, command-r-plus, firefunction-v2. Older or
// non-tool-capable models return an error from the Ollama daemon
// on a tools-containing request, which we propagate as a Go error.
// Callers routing the Implementer to a non-tool-capable local model
// should either swap the model OR keep provider:ollama and fall
// back to the legacy NEED_FILES path (which Implementer.Run does
// automatically when CompleteWithTools is not implemented — this
// PR changes that by *adding* CompleteWithTools to Ollama, so the
// fallback is no longer automatic. See the model compatibility
// doc in config/models.local.yaml).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// ollamaToolReq is the chat request shape for tool-aware calls.
// Same /api/chat endpoint, but with a `tools` field and messages
// that can carry tool_calls (assistant) or be role="tool" (results).
type ollamaToolReq struct {
	Model    string              `json:"model"`
	Stream   bool                `json:"stream"`
	Options  ollamaOptions       `json:"options,omitempty"`
	Messages []ollamaToolMessage `json:"messages"`
	Tools    []ollamaTool        `json:"tools,omitempty"`
}

// ollamaToolMessage matches Ollama's chat message schema for
// tool-aware conversations. Fields are omitempty so each message
// type serializes cleanly:
//
//	system: {role, content}
//	user:   {role, content}
//	assistant with text only: {role, content}
//	assistant with tool calls: {role, content?, tool_calls}
//	tool result: {role: "tool", content}
type ollamaToolMessage struct {
	Role      string           `json:"role"`
	Content   string           `json:"content,omitempty"`
	ToolCalls []ollamaToolCall `json:"tool_calls,omitempty"`
}

// ollamaToolCall is Ollama's function-call shape. No ID field —
// correlation is positional.
type ollamaToolCall struct {
	Function ollamaToolCallFunction `json:"function"`
}

type ollamaToolCallFunction struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// ollamaTool is the OpenAI-style tool schema Ollama expects.
type ollamaTool struct {
	Type     string             `json:"type"` // always "function"
	Function ollamaToolFunction `json:"function"`
}

type ollamaToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// ollamaToolResp is the response shape for a tool-aware chat call.
type ollamaToolResp struct {
	Model   string `json:"model"`
	Message struct {
		Role      string           `json:"role"`
		Content   string           `json:"content"`
		ToolCalls []ollamaToolCall `json:"tool_calls,omitempty"`
	} `json:"message"`
	DoneReason      string `json:"done_reason"`
	PromptEvalCount int    `json:"prompt_eval_count"`
	EvalCount       int    `json:"eval_count"`

	Error string `json:"error,omitempty"`
}

// CompleteWithTools implements ToolAwareProvider for the Ollama
// backend. The Implementer's agentic loop (agents/implementer_tools.go)
// dispatches here when the router hands it an *Ollama for the
// implementer role.
//
// Known failure modes + how we handle them:
//
//   * Model doesn't support tool use. Ollama's daemon returns a
//     400-ish error with a descriptive message. We propagate the
//     error with a hint to switch models or tiers.
//
//   * Model emits malformed tool_calls. We let the JSON decoder
//     fail and return a clear error — the caller's retry loop in
//     the Implementer can decide what to do.
//
//   * Model ignores the tools and just writes prose describing
//     what it would do. We treat that as stop_reason=end_turn
//     with the prose as Content. The Implementer's
//     validateDiffResponse then catches the missing `diff --git`
//     and surfaces a clear error.
func (o *Ollama) CompleteWithTools(ctx context.Context, r ToolAwareRequest) (ToolAwareResponse, error) {
	maxTok := r.MaxTokens
	if maxTok == 0 {
		maxTok = o.maxTokens
	}
	temp := r.Temperature
	if temp == 0 {
		temp = o.temperature
	}

	msgs, err := toOllamaMessages(r.System, r.Messages)
	if err != nil {
		return ToolAwareResponse{}, fmt.Errorf("ollama tools: marshal messages: %w", err)
	}

	body, err := json.Marshal(ollamaToolReq{
		Model:    o.model,
		Stream:   false,
		Options:  ollamaOptions{NumPredict: maxTok, Temperature: temp},
		Messages: msgs,
		Tools:    toOllamaTools(r.Tools),
	})
	if err != nil {
		return ToolAwareResponse{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.endpoint+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return ToolAwareResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := o.http.Do(req)
	if err != nil {
		return ToolAwareResponse{}, fmt.Errorf("ollama tools: %w", err)
	}
	defer resp.Body.Close()

	// Ollama surfaces tool-use errors either as HTTP 4xx or as a
	// 200 OK with an "error" field in the body. Handle both.
	if resp.StatusCode >= 400 {
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(resp.Body)
		return ToolAwareResponse{}, fmt.Errorf("ollama tools: %s: %s", resp.Status, buf.String())
	}

	var out ollamaToolResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return ToolAwareResponse{}, fmt.Errorf("ollama tools decode: %w", err)
	}
	if out.Error != "" {
		return ToolAwareResponse{}, fmt.Errorf("ollama tools: %s — hint: not all Ollama models support tool use; try qwen2.5-coder:14b, llama3.1, or mistral-nemo", out.Error)
	}

	return fromOllamaResponse(&out), nil
}

// toOllamaTools maps aidev's internal ToolDefinition slice to the
// OpenAI-style {type: "function", function: {...}} shape Ollama
// expects. The InputSchema json.RawMessage passes through verbatim
// as the `parameters` field — aidev hand-writes these schemas
// specifically to be portable between Anthropic and OpenAI styles.
func toOllamaTools(defs []ToolDefinition) []ollamaTool {
	if len(defs) == 0 {
		return nil
	}
	out := make([]ollamaTool, 0, len(defs))
	for _, d := range defs {
		out = append(out, ollamaTool{
			Type: "function",
			Function: ollamaToolFunction{
				Name:        d.Name,
				Description: d.Description,
				Parameters:  d.InputSchema,
			},
		})
	}
	return out
}

// toOllamaMessages translates the internal ToolMessage slice to
// Ollama's wire shape. This is where the Anthropic-vs-OpenAI style
// gap shows up: a user ToolMessage carrying tool_result content
// blocks fans out to one or more role="tool" messages on the
// Ollama side.
func toOllamaMessages(system string, in []ToolMessage) ([]ollamaToolMessage, error) {
	out := make([]ollamaToolMessage, 0, len(in)+1)
	if system != "" {
		out = append(out, ollamaToolMessage{
			Role:    "system",
			Content: system,
		})
	}
	for _, m := range in {
		switch m.Role {
		case "user":
			// A user turn with a single text block collapses to a
			// plain {role:"user", content} message. A user turn
			// with tool_result blocks fans out to one role="tool"
			// message per block. A mixed turn (both text and
			// tool_result) becomes one "user" + one or more
			// "tool" messages; unusual but supported.
			var textContent string
			var toolResults []ollamaToolMessage
			for _, b := range m.Content {
				switch b.Type {
				case "text":
					textContent = b.Text
				case "tool_result":
					// Ollama's tool message is just {role:"tool", content}
					// with no correlation ID — positional matching
					// against the preceding assistant's tool_calls.
					content := b.ToolResultContent
					if b.ToolResultIsError {
						content = "error: " + content
					}
					toolResults = append(toolResults, ollamaToolMessage{
						Role:    "tool",
						Content: content,
					})
				default:
					return nil, fmt.Errorf("user message: unknown content block type %q", b.Type)
				}
			}
			if textContent != "" {
				out = append(out, ollamaToolMessage{Role: "user", Content: textContent})
			}
			out = append(out, toolResults...)

		case "assistant":
			// An assistant turn either carries text + tool_use
			// blocks (model called tools) or just text (terminal
			// response). Collapse the text blocks into one string
			// because Ollama's assistant content is a plain
			// string, not an array.
			msg := ollamaToolMessage{Role: "assistant"}
			var textBuf bytes.Buffer
			for _, b := range m.Content {
				switch b.Type {
				case "text":
					textBuf.WriteString(b.Text)
				case "tool_use":
					msg.ToolCalls = append(msg.ToolCalls, ollamaToolCall{
						Function: ollamaToolCallFunction{
							Name:      b.ToolName,
							Arguments: b.ToolInput,
						},
					})
				default:
					return nil, fmt.Errorf("assistant message: unknown content block type %q", b.Type)
				}
			}
			msg.Content = textBuf.String()
			out = append(out, msg)

		default:
			return nil, fmt.Errorf("unknown message role %q", m.Role)
		}
	}
	return out, nil
}

// fromOllamaResponse translates an Ollama chat response into
// aidev's internal ToolAwareResponse. Synthesizes correlation IDs
// for tool_calls (Ollama's wire format omits them) using the
// position in the tool_calls array.
//
// CONTENT-EMBEDDED TOOL CALL FALLBACK (v0.5a):
// Some tool-capable Ollama models — qwen2.5-coder:14b is the
// primary offender I've seen in the wild — sometimes emit tool
// calls as a JSON object in message.content instead of
// populating the structured tool_calls field. The daemon
// faithfully passes the content string through, and our caller
// gets a response with empty tool_calls and content that looks
// like `{"name": "read_file", "arguments": {"path": "foo.go"}}`.
//
// Without a fallback, we'd treat this as stop_reason=end_turn
// and pass the JSON through to the Implementer's diff
// validator, which rejects anything that doesn't start with
// `diff --git`. The result is a hard failure with no recovery
// path — exactly the issue aidev hit on the v0.5 + qwen
// smoke-test run.
//
// The fix: if tool_calls is empty AND content parses as a
// recognizable tool-call shape, synthesize a ToolUse from the
// content and treat the response as stop_reason=tool_use. We
// accept three field-name variants that cover ~all
// OpenAI/Anthropic-compatible models that botch the structured
// output:
//
//	{"name": "...", "arguments": {...}}
//	{"function": "...", "parameters": {...}}
//	{"tool": "...", "input": {...}}
//
// If content is non-JSON or doesn't match any of these shapes,
// we fall through to the normal end_turn path and let the
// caller's format-correction retry handle it.
func fromOllamaResponse(resp *ollamaToolResp) ToolAwareResponse {
	var (
		toolUses []ToolUse
		blocks   []ToolContentBlock
	)

	// Content-embedded tool-call salvage. Runs BEFORE the text
	// block is emitted so that if we successfully extract a tool
	// call, the AssistantMessage only contains the synthesized
	// tool_use block and not the raw JSON as text (which would
	// confuse the next turn's prompt).
	embeddedCalls := extractEmbeddedToolCalls(resp.Message.Content)

	if len(embeddedCalls) == 0 && resp.Message.Content != "" {
		blocks = append(blocks, ToolContentBlock{
			Type: "text",
			Text: resp.Message.Content,
		})
	}

	// Structured tool_calls from the Ollama response.
	for i, call := range resp.Message.ToolCalls {
		id := "ollama_" + strconv.Itoa(i)
		toolUses = append(toolUses, ToolUse{
			ID:    id,
			Name:  call.Function.Name,
			Input: call.Function.Arguments,
		})
		blocks = append(blocks, ToolContentBlock{
			Type:      "tool_use",
			ToolUseID: id,
			ToolName:  call.Function.Name,
			ToolInput: call.Function.Arguments,
		})
	}

	// Content-embedded tool_calls salvaged above. ID numbering
	// continues past any real tool_calls so there's no collision.
	for i, call := range embeddedCalls {
		id := "ollama_embedded_" + strconv.Itoa(len(resp.Message.ToolCalls)+i)
		toolUses = append(toolUses, ToolUse{
			ID:    id,
			Name:  call.Name,
			Input: call.Arguments,
		})
		blocks = append(blocks, ToolContentBlock{
			Type:      "tool_use",
			ToolUseID: id,
			ToolName:  call.Name,
			ToolInput: call.Arguments,
		})
	}

	// Derive a stop_reason in the same vocabulary the Anthropic
	// path uses so the caller doesn't need provider-specific
	// switches. Ollama's done_reason is "stop" on a clean end,
	// "length" on max_tokens, or empty on some daemon versions;
	// the presence of tool_calls (real or salvaged from content)
	// overrides to "tool_use".
	stopReason := "end_turn"
	if len(toolUses) > 0 {
		stopReason = "tool_use"
	} else if resp.DoneReason == "length" {
		stopReason = "max_tokens"
	}

	// Content that survived the embedded-parse salvage is the
	// one we expose to the caller. If we extracted tool calls,
	// the content string is cleared (since the "text" was
	// actually the tool call JSON, which is already represented
	// as ToolUses) so the Implementer's validator doesn't
	// mistake it for a diff.
	content := resp.Message.Content
	if len(embeddedCalls) > 0 {
		content = ""
	}

	return ToolAwareResponse{
		Content:    content,
		ToolUses:   toolUses,
		StopReason: stopReason,
		Model:      resp.Model,
		Usage: Usage{
			InputTokens:  resp.PromptEvalCount,
			OutputTokens: resp.EvalCount,
		},
		AssistantMessage: ToolMessage{
			Role:    "assistant",
			Content: blocks,
		},
	}
}

// embeddedToolCall is the parsed shape of a content-embedded
// tool call. Used internally by extractEmbeddedToolCalls; the
// caller converts these into proper ToolUses.
type embeddedToolCall struct {
	Name      string
	Arguments json.RawMessage
}

// extractEmbeddedToolCalls scans Ollama's content string for
// JSON objects that look like tool calls in any of the common
// wire shapes, and returns parsed results. Returns nil when the
// content isn't a tool call (most responses).
//
// The parser is conservative: it only treats content as a tool
// call when the ENTIRE content string is a single JSON object
// with recognizable fields. A string like "I'll read the file.
// {"name": ...}" is NOT salvaged because the model clearly
// mixed prose and JSON, which is a different failure mode and
// should go through format-correction instead.
//
// Accepted shapes:
//
//	{"name": "read_file", "arguments": {"path": "foo.go"}}
//	{"function": "read_file", "parameters": {"path": "foo.go"}}
//	{"tool": "read_file", "input": {"path": "foo.go"}}
//
// A JSON array of such objects is also accepted (some models
// emit multiple tool calls as an array).
func extractEmbeddedToolCalls(content string) []embeddedToolCall {
	trimmed := trimCodeFence(content)
	if trimmed == "" {
		return nil
	}
	// Quick rejection: if the first non-whitespace char isn't
	// `{` or `[`, it can't be a tool-call JSON.
	first := byte(' ')
	for i := 0; i < len(trimmed); i++ {
		if trimmed[i] != ' ' && trimmed[i] != '\t' && trimmed[i] != '\n' && trimmed[i] != '\r' {
			first = trimmed[i]
			break
		}
	}
	if first != '{' && first != '[' {
		return nil
	}

	// Try as a single object first.
	var single map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &single); err == nil {
		if call, ok := parseEmbeddedToolCallObject(single); ok {
			return []embeddedToolCall{call}
		}
	}
	// Try as an array of objects.
	var array []map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &array); err == nil {
		var out []embeddedToolCall
		for _, obj := range array {
			if call, ok := parseEmbeddedToolCallObject(obj); ok {
				out = append(out, call)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

// parseEmbeddedToolCallObject tries every accepted shape on
// a single JSON object and returns the first match.
func parseEmbeddedToolCallObject(obj map[string]json.RawMessage) (embeddedToolCall, bool) {
	// Shape 1: {"name": "...", "arguments": {...}}
	if nameRaw, ok := obj["name"]; ok {
		if argsRaw, ok := obj["arguments"]; ok {
			if name, ok := jsonString(nameRaw); ok && name != "" {
				return embeddedToolCall{Name: name, Arguments: argsRaw}, true
			}
		}
	}
	// Shape 2: {"function": "...", "parameters": {...}}
	if nameRaw, ok := obj["function"]; ok {
		if argsRaw, ok := obj["parameters"]; ok {
			if name, ok := jsonString(nameRaw); ok && name != "" {
				return embeddedToolCall{Name: name, Arguments: argsRaw}, true
			}
		}
	}
	// Shape 3: {"tool": "...", "input": {...}}
	if nameRaw, ok := obj["tool"]; ok {
		if argsRaw, ok := obj["input"]; ok {
			if name, ok := jsonString(nameRaw); ok && name != "" {
				return embeddedToolCall{Name: name, Arguments: argsRaw}, true
			}
		}
	}
	return embeddedToolCall{}, false
}

// jsonString decodes a json.RawMessage into a Go string when it
// is a JSON string. Returns ok=false when the raw message is
// anything else (object, array, number, etc.).
func jsonString(raw json.RawMessage) (string, bool) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, true
	}
	return "", false
}

// trimCodeFence strips a leading and trailing ``` fence if
// present. Used before JSON parsing because some models wrap
// their tool-call JSON in a Markdown code fence despite being
// told not to.
func trimCodeFence(s string) string {
	lines := splitLines(s)
	if len(lines) == 0 {
		return s
	}
	// Strip leading fence.
	if strings.HasPrefix(strings.TrimSpace(lines[0]), "```") {
		lines = lines[1:]
	}
	// Strip trailing fence.
	if n := len(lines); n > 0 && strings.HasPrefix(strings.TrimSpace(lines[n-1]), "```") {
		lines = lines[:n-1]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// splitLines is a strings.Split on "\n" kept in a helper so the
// trimCodeFence logic reads a little more naturally.
func splitLines(s string) []string {
	return strings.Split(s, "\n")
}
