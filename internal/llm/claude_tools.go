package llm

// Anthropic Messages API tool-use support for the Claude provider.
// This file implements ToolAwareProvider on top of the existing
// REST client in claude.go.
//
// The wire format for Anthropic tool use:
//
//	Request body:
//	  {
//	    "model": "...",
//	    "max_tokens": N,
//	    "system": "...",
//	    "tools": [ { "name": "read_file", "description": "...",
//	                 "input_schema": {...JSON Schema...} } ],
//	    "messages": [
//	      { "role": "user", "content": [ { "type": "text", "text": "..." } ] },
//	      { "role": "assistant", "content": [
//	          { "type": "text", "text": "I'll read it." },
//	          { "type": "tool_use", "id": "toolu_1",
//	            "name": "read_file", "input": {"path": "foo.go"} }
//	        ] },
//	      { "role": "user", "content": [
//	          { "type": "tool_result", "tool_use_id": "toolu_1",
//	            "content": "<file body>", "is_error": false }
//	        ] }
//	    ]
//	  }
//
//	Response body content blocks for assistant turns:
//	  [ { "type": "text", "text": "..." },
//	    { "type": "tool_use", "id": "toolu_xyz",
//	      "name": "glob", "input": {"pattern": "**/*.go"} } ]
//
//	stop_reason: "end_turn" when done, "tool_use" when pending tool calls.
//
// The encoding is isomorphic to our internal ToolMessage /
// ToolContentBlock representation so translation is purely mechanical.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// claudeToolsReq is the request body for a tool-aware Messages call.
// We reuse the existing claudeReq's system/model/temperature fields
// but need a richer messages shape because tool-use conversations
// have content blocks, not plain strings. Marshaled directly to JSON
// — the outer Messages field shadows claudeReq.Messages.
type claudeToolsReq struct {
	Model       string               `json:"model"`
	MaxTokens   int                  `json:"max_tokens"`
	Temperature float64              `json:"temperature"`
	System      string               `json:"system,omitempty"`
	Tools       []ToolDefinition     `json:"tools,omitempty"`
	Messages    []claudeToolsMessage `json:"messages"`
}

// claudeToolsMessage is a single turn with an array of content
// blocks. This is how Anthropic represents multi-block turns
// (assistant text + tool_use, or user text + tool_result).
type claudeToolsMessage struct {
	Role    string              `json:"role"`
	Content []claudeContentBlock `json:"content"`
}

// claudeContentBlock is the JSON shape of a single content block.
// The field tags make `omitempty` do most of the discrimination
// work — tool_use blocks get Name/Input populated, tool_result
// blocks get ToolUseID/ResultContent, text blocks get Text.
type claudeContentBlock struct {
	Type string `json:"type"`

	// text blocks
	Text string `json:"text,omitempty"`

	// tool_use blocks
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result blocks
	ToolUseID     string `json:"tool_use_id,omitempty"`
	ResultContent string `json:"content,omitempty"`
	IsError       bool   `json:"is_error,omitempty"`
}

// claudeToolsResp is the response body for a tool-aware Messages
// call. Only the fields we actually use are represented.
type claudeToolsResp struct {
	Content    []claudeContentBlock `json:"content"`
	Model      string               `json:"model"`
	StopReason string               `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// CompleteWithTools implements ToolAwareProvider for the Claude
// backend. Translates the provider-neutral ToolAwareRequest into
// Anthropic's wire format, POSTs it, and decodes the response back
// into a ToolAwareResponse.
//
// The function is a straight-line translation — no state, no
// retries. The caller's loop (in agents/toolloop.go) is responsible
// for iterating turns until stop_reason is "end_turn".
func (c *Claude) CompleteWithTools(ctx context.Context, r ToolAwareRequest) (ToolAwareResponse, error) {
	if c.apiKey == "" {
		return ToolAwareResponse{}, fmt.Errorf("claude: ANTHROPIC_API_KEY not set")
	}

	maxTok := r.MaxTokens
	if maxTok == 0 {
		maxTok = c.maxTokens
	}
	temp := r.Temperature
	if temp == 0 {
		temp = c.temperature
	}

	// Translate internal ToolMessages -> Anthropic content blocks.
	msgs, err := toClaudeMessages(r.Messages)
	if err != nil {
		return ToolAwareResponse{}, fmt.Errorf("claude tools: marshal messages: %w", err)
	}

	body, err := json.Marshal(claudeToolsReq{
		Model:       c.model,
		MaxTokens:   maxTok,
		Temperature: temp,
		System:      r.System,
		Tools:       r.Tools,
		Messages:    msgs,
	})
	if err != nil {
		return ToolAwareResponse{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return ToolAwareResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := c.http.Do(req)
	if err != nil {
		return ToolAwareResponse{}, fmt.Errorf("claude tools: %w", err)
	}
	defer resp.Body.Close()

	var out claudeToolsResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return ToolAwareResponse{}, fmt.Errorf("claude tools decode: %w", err)
	}
	if out.Error != nil {
		return ToolAwareResponse{}, fmt.Errorf("claude tools %s: %s", out.Error.Type, out.Error.Message)
	}

	return fromClaudeResponse(&out), nil
}

// toClaudeMessages translates aidev's internal ToolMessage slice
// into the Anthropic wire shape. Unknown block types are rejected
// with an error — better to fail loudly than to drop content the
// caller expected us to send.
func toClaudeMessages(in []ToolMessage) ([]claudeToolsMessage, error) {
	out := make([]claudeToolsMessage, 0, len(in))
	for _, m := range in {
		blocks := make([]claudeContentBlock, 0, len(m.Content))
		for _, b := range m.Content {
			switch b.Type {
			case "text":
				blocks = append(blocks, claudeContentBlock{
					Type: "text",
					Text: b.Text,
				})
			case "tool_use":
				blocks = append(blocks, claudeContentBlock{
					Type:  "tool_use",
					ID:    b.ToolUseID,
					Name:  b.ToolName,
					Input: b.ToolInput,
				})
			case "tool_result":
				blocks = append(blocks, claudeContentBlock{
					Type:          "tool_result",
					ToolUseID:     b.ToolUseID,
					ResultContent: b.ToolResultContent,
					IsError:       b.ToolResultIsError,
				})
			default:
				return nil, fmt.Errorf("unknown content block type %q", b.Type)
			}
		}
		out = append(out, claudeToolsMessage{
			Role:    m.Role,
			Content: blocks,
		})
	}
	return out, nil
}

// fromClaudeResponse translates an Anthropic response into aidev's
// ToolAwareResponse. Builds the AssistantMessage as a ToolMessage
// that the caller can append to its own history for the next turn.
func fromClaudeResponse(resp *claudeToolsResp) ToolAwareResponse {
	var (
		textBuf  bytes.Buffer
		toolUses []ToolUse
		blocks   []ToolContentBlock
	)
	for _, b := range resp.Content {
		switch b.Type {
		case "text":
			textBuf.WriteString(b.Text)
			blocks = append(blocks, ToolContentBlock{
				Type: "text",
				Text: b.Text,
			})
		case "tool_use":
			toolUses = append(toolUses, ToolUse{
				ID:    b.ID,
				Name:  b.Name,
				Input: b.Input,
			})
			blocks = append(blocks, ToolContentBlock{
				Type:      "tool_use",
				ToolUseID: b.ID,
				ToolName:  b.Name,
				ToolInput: b.Input,
			})
		}
	}
	return ToolAwareResponse{
		Content:    textBuf.String(),
		ToolUses:   toolUses,
		StopReason: resp.StopReason,
		Model:      resp.Model,
		Usage: Usage{
			InputTokens:  resp.Usage.InputTokens,
			OutputTokens: resp.Usage.OutputTokens,
		},
		AssistantMessage: ToolMessage{
			Role:    "assistant",
			Content: blocks,
		},
	}
}
