package llm

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// Compile-time guards: every middleware in this package must declare
// CompleteWithTools so a chain of middlewares around a tool-aware base
// provider still satisfies ToolAwareProvider at the outermost layer.
// If any of these lines stops compiling, the Implementer's type
// assertion at internal/agents/implementer.go will start falling
// through to the legacy picker path unconditionally, which is the bug
// this file exists to prevent.
var (
	_ ToolAwareProvider = (*RetryWrapper)(nil)
	_ ToolAwareProvider = (*CircuitBreaker)(nil)
	_ ToolAwareProvider = (*telemetryWrapper)(nil)
)

// stubToolProvider is a ToolAwareProvider stub for tests that need to
// observe whether CompleteWithTools was forwarded to the inner
// provider through a middleware chain.
type stubToolProvider struct {
	name      string
	resp      ToolAwareResponse
	err       error
	callCount int
}

func (s *stubToolProvider) Name() string { return s.name }

func (s *stubToolProvider) Complete(ctx context.Context, req Request) (Response, error) {
	return Response{}, nil
}

func (s *stubToolProvider) CompleteWithTools(ctx context.Context, req ToolAwareRequest) (ToolAwareResponse, error) {
	s.callCount++
	return s.resp, s.err
}

func TestMiddlewareChainForwardsCompleteWithTools(t *testing.T) {
	inner := &stubToolProvider{
		name: "ollama:qwen2.5-coder:14b",
		resp: ToolAwareResponse{
			Content:    "ok",
			StopReason: "end_turn",
			Usage:      Usage{InputTokens: 42, OutputTokens: 17},
			AssistantMessage: ToolMessage{
				Role:    "assistant",
				Content: []ToolContentBlock{{Type: "text", Text: "ok"}},
			},
		},
	}

	rec := NewRecorder()
	chain := ProviderWithMiddleware(inner,
		WithRetry(RetryConfig{MaxAttempts: 2, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, Backoff: BackoffLinear}),
		WithCircuitBreaker(3, time.Second),
		WithTelemetry(rec),
	)

	ta, ok := chain.(ToolAwareProvider)
	if !ok {
		t.Fatalf("chain does not implement ToolAwareProvider: %T", chain)
	}

	ctx := WithGate(context.Background(), "implementer")
	resp, err := ta.CompleteWithTools(ctx, ToolAwareRequest{
		System: "s",
		Tools:  []ToolDefinition{{Name: "read_file", Description: "", InputSchema: json.RawMessage(`{}`)}},
		Messages: []ToolMessage{{Role: "user", Content: []ToolContentBlock{{Type: "text", Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("CompleteWithTools: %v", err)
	}
	if inner.callCount != 1 {
		t.Errorf("expected inner callCount=1, got %d", inner.callCount)
	}
	if resp.Content != "ok" {
		t.Errorf("resp not forwarded: %+v", resp)
	}

	got := rec.Summarize()
	if len(got) != 1 || got[0].Gate != "implementer" || got[0].InputTokens != 42 || got[0].OutputTokens != 17 || got[0].Calls != 1 {
		t.Errorf("telemetry row wrong: %+v", got)
	}
}

func TestMiddlewareChainReturnsErrToolsNotSupported(t *testing.T) {
	// stubProvider (telemetry_test.go) is a plain Provider with no
	// CompleteWithTools — exactly the case we need to exercise.
	inner := &stubProvider{name: "fake:plain"}

	rec := NewRecorder()
	chain := ProviderWithMiddleware(inner,
		WithRetry(RetryConfig{MaxAttempts: 2, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, Backoff: BackoffLinear}),
		WithCircuitBreaker(3, time.Second),
		WithTelemetry(rec),
	)

	ta, ok := chain.(ToolAwareProvider)
	if !ok {
		t.Fatalf("chain must still satisfy ToolAwareProvider even when inner does not: %T", chain)
	}

	_, err := ta.CompleteWithTools(context.Background(), ToolAwareRequest{})
	if !errors.Is(err, ErrToolsNotSupported) {
		t.Fatalf("expected ErrToolsNotSupported, got %v", err)
	}

	// No CallRecord should be emitted for a capability-check failure:
	// there was no real LLM call to attribute.
	if got := rec.Summarize(); len(got) != 0 {
		t.Errorf("expected no telemetry records for unsupported path, got %+v", got)
	}
}

func TestRetryWrapperCompleteWithToolsRetriesRetryable(t *testing.T) {
	// First call fails with a retryable error, second call succeeds.
	attempts := 0
	inner := &flakyToolProvider{
		name: "ollama:test",
		fn: func() (ToolAwareResponse, error) {
			attempts++
			if attempts == 1 {
				return ToolAwareResponse{}, errors.New("connection refused")
			}
			return ToolAwareResponse{Content: "done"}, nil
		},
	}
	rw := NewRetryWrapper(inner, RetryConfig{
		MaxAttempts: 3,
		BaseDelay:   time.Millisecond,
		MaxDelay:    time.Millisecond,
		Backoff:     BackoffLinear,
	})
	resp, err := rw.CompleteWithTools(context.Background(), ToolAwareRequest{})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if resp.Content != "done" || attempts != 2 {
		t.Errorf("resp=%+v attempts=%d", resp, attempts)
	}
}

// flakyToolProvider is a ToolAwareProvider whose CompleteWithTools
// behaviour is supplied by a closure — keeps retry-logic tests
// self-contained without inventing a new mocking framework.
type flakyToolProvider struct {
	name string
	fn   func() (ToolAwareResponse, error)
}

func (f *flakyToolProvider) Name() string { return f.name }
func (f *flakyToolProvider) Complete(ctx context.Context, req Request) (Response, error) {
	return Response{}, nil
}
func (f *flakyToolProvider) CompleteWithTools(ctx context.Context, req ToolAwareRequest) (ToolAwareResponse, error) {
	return f.fn()
}
