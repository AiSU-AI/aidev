package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// Claude talks to the Anthropic Messages API directly over HTTP. We avoid
// pulling in anthropic-sdk-go so aidev's build graph stays small and
// reproducible; the Messages API is stable and small.
type Claude struct {
	apiKey      string
	model       string
	maxTokens   int
	temperature float64
	endpoint    string
	http        *http.Client
}

// NewClaude constructs a Claude provider. The API key is read from the
// ANTHROPIC_API_KEY environment variable. timeout may be 0; in that
// case DefaultHTTPTimeout is used.
func NewClaude(model string, maxTokens int, temperature float64, timeout time.Duration) *Claude {
	if timeout <= 0 {
		timeout = DefaultHTTPTimeout
	}
	return &Claude{
		apiKey:      os.Getenv("ANTHROPIC_API_KEY"),
		model:       model,
		maxTokens:   maxTokens,
		temperature: temperature,
		endpoint:    "https://api.anthropic.com/v1/messages",
		http:        &http.Client{Timeout: timeout},
	}
}

func (c *Claude) Name() string { return "anthropic:" + c.model }

// HasKey reports whether ANTHROPIC_API_KEY was set at construction time. The
// TUI uses this to display provider status without making a network call.
func (c *Claude) HasKey() bool { return c.apiKey != "" }

type claudeReq struct {
	Model       string          `json:"model"`
	MaxTokens   int             `json:"max_tokens"`
	Temperature float64         `json:"temperature"`
	System      string          `json:"system,omitempty"`
	Messages    []claudeMessage `json:"messages"`
}

type claudeMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type claudeResp struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Model string `json:"model"`
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Stream issues a streaming Messages API call and delivers SSE events
// as StreamChunks on the returned channel. Implements the Streamer
// capability interface for consumers that want live incremental
// output (the `-stream` headless flag, a future TUI progress view).
// Cancelling ctx aborts the stream.
func (c *Claude) Stream(ctx context.Context, r Request) (<-chan StreamChunk, error) {
	if c.apiKey == "" {
		return nil, fmt.Errorf("claude: ANTHROPIC_API_KEY not set")
	}

	msgs := make([]claudeMessage, 0, len(r.Messages))
	for _, m := range r.Messages {
		msgs = append(msgs, claudeMessage{Role: m.Role, Content: m.Content})
	}
	maxTok := r.MaxTokens
	if maxTok == 0 {
		maxTok = c.maxTokens
	}
	temp := r.Temperature
	if temp == 0 {
		temp = c.temperature
	}

	// Marshal the existing request shape with stream: true. We use an
	// anonymous struct instead of adding a Stream field to claudeReq
	// because Complete() would then have to explicitly set it to false.
	body, err := json.Marshal(struct {
		claudeReq
		Stream bool `json:"stream"`
	}{
		claudeReq: claudeReq{
			Model:       c.model,
			MaxTokens:   maxTok,
			Temperature: temp,
			System:      r.System,
			Messages:    msgs,
		},
		Stream: true,
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("claude stream: %w", err)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		return nil, fmt.Errorf("claude stream: %s", resp.Status)
	}

	out := make(chan StreamChunk, 16)
	go parseClaudeSSE(resp, out)
	return out, nil
}

// parseClaudeSSE reads an Anthropic SSE stream off the given response
// body and emits StreamChunks until EOF. Anthropic's Messages streaming
// format is a sequence of `event: <type>` + `data: <json>` lines
// separated by blank lines. We care about three event types:
//
//	content_block_delta      incremental text output
//	message_stop             end of stream
//	error                    fatal error mid-stream
//
// All other frames (message_start, ping, content_block_start, etc.) are
// silently ignored.
func parseClaudeSSE(resp *http.Response, out chan<- StreamChunk) {
	defer resp.Body.Close()
	defer close(out)

	scanner := bufio.NewScanner(resp.Body)
	// SSE frames can be bigger than bufio's default 64 KB buffer. Bump
	// to 1 MB so long message_delta frames don't trip the scanner.
	scanner.Buffer(make([]byte, 0, 1024), 1024*1024)

	var (
		eventType string
		dataLine  string
	)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			// Blank line — end of frame. Dispatch and reset.
			if dataLine != "" {
				dispatchClaudeEvent(eventType, dataLine, out)
			}
			eventType = ""
			dataLine = ""
			continue
		}
		switch {
		case strings.HasPrefix(line, "event: "):
			eventType = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			dataLine = strings.TrimPrefix(line, "data: ")
		}
	}
	if err := scanner.Err(); err != nil {
		out <- StreamChunk{Done: true, Err: fmt.Errorf("claude sse scan: %w", err)}
	}
}

// dispatchClaudeEvent decodes one SSE frame and emits the corresponding
// StreamChunk(s) when the event type is meaningful. Unknown types and
// malformed JSON are silently dropped — the Anthropic stream includes
// frames we don't care about and we don't want those to abort the
// consumer.
func dispatchClaudeEvent(eventType, data string, out chan<- StreamChunk) {
	switch eventType {
	case "content_block_delta":
		var frame struct {
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		}
		if err := json.Unmarshal([]byte(data), &frame); err != nil {
			return
		}
		if frame.Delta.Type == "text_delta" && frame.Delta.Text != "" {
			out <- StreamChunk{Text: frame.Delta.Text}
		}
	case "message_stop":
		out <- StreamChunk{Done: true}
	case "error":
		var frame struct {
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal([]byte(data), &frame)
		out <- StreamChunk{Done: true, Err: fmt.Errorf("claude stream error: %s: %s", frame.Error.Type, frame.Error.Message)}
	}
}

// Complete issues a single non-streaming Messages API call.
func (c *Claude) Complete(ctx context.Context, r Request) (Response, error) {
	if c.apiKey == "" {
		return Response{}, fmt.Errorf("claude: ANTHROPIC_API_KEY not set")
	}

	msgs := make([]claudeMessage, 0, len(r.Messages))
	for _, m := range r.Messages {
		msgs = append(msgs, claudeMessage{Role: m.Role, Content: m.Content})
	}

	maxTok := r.MaxTokens
	if maxTok == 0 {
		maxTok = c.maxTokens
	}
	temp := r.Temperature
	if temp == 0 {
		temp = c.temperature
	}

	body, err := json.Marshal(claudeReq{
		Model:       c.model,
		MaxTokens:   maxTok,
		Temperature: temp,
		System:      r.System,
		Messages:    msgs,
	})
	if err != nil {
		return Response{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return Response{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := c.http.Do(req)
	if err != nil {
		return Response{}, fmt.Errorf("claude request: %w", err)
	}
	defer resp.Body.Close()

	var out claudeResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Response{}, fmt.Errorf("claude decode: %w", err)
	}
	if out.Error != nil {
		return Response{}, fmt.Errorf("claude %s: %s", out.Error.Type, out.Error.Message)
	}

	// The Messages API returns a list of content blocks. We concatenate the
	// text blocks; tool-use blocks are ignored for now because the v0.1
	// Scout and Critic agents do not use tools.
	var content bytes.Buffer
	for _, b := range out.Content {
		if b.Type == "text" {
			content.WriteString(b.Text)
		}
	}

	return Response{
		Content: content.String(),
		Model:   out.Model,
		Usage: Usage{
			InputTokens:  out.Usage.InputTokens,
			OutputTokens: out.Usage.OutputTokens,
		},
	}, nil
}
