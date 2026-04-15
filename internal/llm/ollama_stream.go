package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// ollamaStreamResp represents a single chunk from Ollama's streaming API
type ollamaStreamResp struct {
	Message struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"message"`
	Done            bool   `json:"done"`
	Model           string `json:"model"`
	PromptEvalCount int    `json:"prompt_eval_count"`
	EvalCount       int    `json:"eval_count"`
}

// Stream implements the Streamer interface for Ollama
func (o *Ollama) Stream(ctx context.Context, req Request) (<-chan StreamChunk, error) {
	// Build messages
	msgs := make([]ollamaMessage, 0, len(req.Messages)+1)
	if req.System != "" {
		msgs = append(msgs, ollamaMessage{Role: "system", Content: req.System})
	}
	for _, m := range req.Messages {
		msgs = append(msgs, ollamaMessage{Role: m.Role, Content: m.Content})
	}

	// Set parameters
	maxTok := req.MaxTokens
	if maxTok == 0 {
		maxTok = o.maxTokens
	}
	temp := req.Temperature
	if temp == 0 {
		temp = o.temperature
	}

	// Build request body
	body, err := json.Marshal(ollamaChatReq{
		Model:    o.model,
		Messages: msgs,
		Stream:   true,
		Options:  ollamaOptions{NumPredict: maxTok, Temperature: temp},
	})
	if err != nil {
		return nil, err
	}

	// Create HTTP request
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.endpoint+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	// Make request
	resp, err := o.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("ollama stream: %w", err)
	}

	if resp.StatusCode >= 400 {
		resp.Body.Close()
		return nil, fmt.Errorf("ollama stream: %s", resp.Status)
	}

	// Create output channel
	ch := make(chan StreamChunk, 10) // Buffered channel for smooth streaming

	// Start streaming goroutine
	go func() {
		defer resp.Body.Close()
		defer close(ch)

		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			select {
			case <-ctx.Done():
				// Context cancelled
				ch <- StreamChunk{Err: ctx.Err(), Done: true}
				return
			default:
			}

			line := scanner.Bytes()
			if len(line) == 0 {
				continue
			}

			var chunk ollamaStreamResp
			if err := json.Unmarshal(line, &chunk); err != nil {
				// Log error but continue streaming
				ch <- StreamChunk{Err: fmt.Errorf("decode error: %w", err), Done: false}
				continue
			}

			// Send chunk
			streamChunk := StreamChunk{
				Text: chunk.Message.Content,
				Done: chunk.Done,
			}

			if chunk.Done {
				// This is the final chunk, include usage info if available
				if chunk.PromptEvalCount > 0 || chunk.EvalCount > 0 {
					// We could enhance StreamChunk to include usage info
					// For now, just mark as done
				}
			}

			select {
			case ch <- streamChunk:
			case <-ctx.Done():
				// Context cancelled during send
				return
			}
		}

		// Check for scanner errors
		if err := scanner.Err(); err != nil {
			select {
			case ch <- StreamChunk{Err: fmt.Errorf("scanner error: %w", err), Done: true}:
			case <-ctx.Done():
			}
			return
		}
	}()

	return ch, nil
}

// StreamWithTools implements streaming for tool-aware conversations
func (o *Ollama) StreamWithTools(ctx context.Context, req ToolAwareRequest) (<-chan StreamChunk, error) {
	// Convert internal format to Ollama format
	messages, err := o.convertToolMessages(req.Messages)
	if err != nil {
		return nil, fmt.Errorf("convert tool messages: %w", err)
	}

	// Convert tools to Ollama format
	tools := make([]ollamaTool, len(req.Tools))
	for i, tool := range req.Tools {
		tools[i] = ollamaTool{
			Type: "function",
			Function: ollamaToolFunction{
				Name:        tool.Name,
				Description: tool.Description,
				Parameters:  tool.InputSchema,
			},
		}
	}

	// Build request
	body, err := json.Marshal(ollamaToolReq{
		Model:    o.model,
		Stream:   true,
		Messages: messages,
		Tools:    tools,
		Options: ollamaOptions{
			NumPredict:  req.MaxTokens,
			Temperature: req.Temperature,
		},
	})
	if err != nil {
		return nil, err
	}

	// Make HTTP request
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.endpoint+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := o.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("ollama tool stream: %w", err)
	}

	if resp.StatusCode >= 400 {
		resp.Body.Close()
		return nil, fmt.Errorf("ollama tool stream: %s", resp.Status)
	}

	// Create output channel
	ch := make(chan StreamChunk, 10)

	// Start streaming goroutine
	go func() {
		defer resp.Body.Close()
		defer close(ch)

		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			select {
			case <-ctx.Done():
				ch <- StreamChunk{Err: ctx.Err(), Done: true}
				return
			default:
			}

			line := scanner.Bytes()
			if len(line) == 0 {
				continue
			}

			var chunk ollamaStreamResp
			if err := json.Unmarshal(line, &chunk); err != nil {
				ch <- StreamChunk{Err: fmt.Errorf("decode error: %w", err), Done: false}
				continue
			}

			// For tool streaming, we need to handle tool_calls in the response
			// This is a simplified version - full implementation would need to
			// accumulate tool calls across multiple chunks
			streamChunk := StreamChunk{
				Text: chunk.Message.Content,
				Done: chunk.Done,
			}

			select {
			case ch <- streamChunk:
			case <-ctx.Done():
				return
			}
		}

		if err := scanner.Err(); err != nil {
			select {
			case ch <- StreamChunk{Err: fmt.Errorf("scanner error: %w", err), Done: true}:
			case <-ctx.Done():
			}
			return
		}
	}()

	return ch, nil
}

// convertToolMessages converts internal ToolMessage format to Ollama format
func (o *Ollama) convertToolMessages(messages []ToolMessage) ([]ollamaToolMessage, error) {
	var ollamaMsgs []ollamaToolMessage

	for _, msg := range messages {
		ollamaMsg := ollamaToolMessage{Role: msg.Role}

		// Handle different content block types
		for _, block := range msg.Content {
			switch block.Type {
			case "text":
				ollamaMsg.Content += block.Text
			case "tool_use":
				// Convert tool_use to Ollama's tool_calls format
				toolCall := ollamaToolCall{
					Function: ollamaToolCallFunction{
						Name:      block.ToolName,
						Arguments: block.ToolInput,
					},
				}
				ollamaMsg.ToolCalls = append(ollamaMsg.ToolCalls, toolCall)
			case "tool_result":
				// Tool results in Ollama are separate messages with role="tool"
				toolResultMsg := ollamaToolMessage{
					Role:    "tool",
					Content: block.ToolResultContent,
				}
				ollamaMsgs = append(ollamaMsgs, toolResultMsg)
			}
		}

		// Only add the message if it has content or tool calls
		if ollamaMsg.Content != "" || len(ollamaMsg.ToolCalls) > 0 {
			ollamaMsgs = append(ollamaMsgs, ollamaMsg)
		}
	}

	return ollamaMsgs, nil
}

// Enhanced Ollama provider with better timeout handling
func NewOllamaWithStreaming(endpoint, model string, maxTokens int, temperature float64) *Ollama {
	if endpoint == "" {
		endpoint = "http://localhost:11434"
	}
	return &Ollama{
		endpoint:    endpoint,
		model:       model,
		maxTokens:   maxTokens,
		temperature: temperature,
		http: &http.Client{
			Timeout: 10 * time.Minute, // Longer timeout for streaming
		},
	}
}
