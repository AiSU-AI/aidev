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

// Ollama is a Provider that talks to a local (or remote) Ollama daemon over
// HTTP. We hand-roll the client instead of pulling in ollama/ollama as a
// dependency — the API is tiny and the import graph of the upstream module
// is not.
type Ollama struct {
	endpoint    string
	model       string
	maxTokens   int
	temperature float64
	http        *http.Client
}

// NewOllama constructs an Ollama provider. endpoint may be empty; if so it
// falls back to AIDEV_OLLAMA_URL and finally http://localhost:11434.
func NewOllama(endpoint, model string, maxTokens int, temperature float64) *Ollama {
	if endpoint == "" {
		endpoint = os.Getenv("AIDEV_OLLAMA_URL")
	}
	if endpoint == "" {
		endpoint = "http://localhost:11434"
	}
	return &Ollama{
		endpoint:    endpoint,
		model:       model,
		maxTokens:   maxTokens,
		temperature: temperature,
		http:        &http.Client{Timeout: 5 * time.Minute},
	}
}

func (o *Ollama) Name() string { return "ollama:" + o.model }

// Health pings the daemon's /api/tags endpoint. It is intentionally a cheap
// HEAD-ish call so the TUI can display a status dot without delay.
func (o *Ollama) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.endpoint+"/api/tags", nil)
	if err != nil {
		return err
	}
	resp, err := o.http.Do(req)
	if err != nil {
		return fmt.Errorf("ollama unreachable at %s: %w", o.endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("ollama returned %s", resp.Status)
	}
	return nil
}

type ollamaChatReq struct {
	Model    string          `json:"model"`
	Messages []ollamaMessage `json:"messages"`
	Stream   bool            `json:"stream"`
	Options  ollamaOptions   `json:"options"`
}

type ollamaMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ollamaOptions struct {
	NumPredict  int     `json:"num_predict,omitempty"`
	Temperature float64 `json:"temperature,omitempty"`
}

type ollamaChatResp struct {
	Message struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"message"`
	Model           string `json:"model"`
	PromptEvalCount int    `json:"prompt_eval_count"`
	EvalCount       int    `json:"eval_count"`
}

// Complete sends a single /api/chat request.
func (o *Ollama) Complete(ctx context.Context, r Request) (Response, error) {
	msgs := make([]ollamaMessage, 0, len(r.Messages)+1)
	if r.System != "" {
		msgs = append(msgs, ollamaMessage{Role: "system", Content: r.System})
	}
	for _, m := range r.Messages {
		msgs = append(msgs, ollamaMessage{Role: m.Role, Content: m.Content})
	}

	maxTok := r.MaxTokens
	if maxTok == 0 {
		maxTok = o.maxTokens
	}
	temp := r.Temperature
	if temp == 0 {
		temp = o.temperature
	}

	body, err := json.Marshal(ollamaChatReq{
		Model:    o.model,
		Messages: msgs,
		Stream:   false,
		Options:  ollamaOptions{NumPredict: maxTok, Temperature: temp},
	})
	if err != nil {
		return Response{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.endpoint+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return Response{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := o.http.Do(req)
	if err != nil {
		return Response{}, fmt.Errorf("ollama chat: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return Response{}, fmt.Errorf("ollama chat: %s", resp.Status)
	}

	var out ollamaChatResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Response{}, fmt.Errorf("ollama decode: %w", err)
	}
	return Response{
		Content: out.Message.Content,
		Model:   out.Model,
		Usage: Usage{
			InputTokens:  out.PromptEvalCount,
			OutputTokens: out.EvalCount,
		},
	}, nil
}
