package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
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
// ANTHROPIC_API_KEY environment variable.
func NewClaude(model string, maxTokens int, temperature float64) *Claude {
	return &Claude{
		apiKey:      os.Getenv("ANTHROPIC_API_KEY"),
		model:       model,
		maxTokens:   maxTokens,
		temperature: temperature,
		endpoint:    "https://api.anthropic.com/v1/messages",
		http:        &http.Client{Timeout: 5 * time.Minute},
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
