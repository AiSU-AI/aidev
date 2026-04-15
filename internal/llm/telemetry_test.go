package llm

import (
	"context"
	"errors"
	"testing"
	"time"
)

// stubProvider is a minimal Provider for exercising the telemetry
// middleware without hitting a real backend.
type stubProvider struct {
	name string
	resp Response
	err  error
}

func (s *stubProvider) Name() string { return s.name }
func (s *stubProvider) Complete(ctx context.Context, req Request) (Response, error) {
	return s.resp, s.err
}

func TestTelemetryRecordsPerGate(t *testing.T) {
	rec := NewRecorder()
	p := WithTelemetry(rec)(&stubProvider{
		name: "ollama:qwen2.5-coder:7b",
		resp: Response{Content: "ok", Usage: Usage{InputTokens: 120, OutputTokens: 45}},
	})

	ctx := context.Background()
	if _, err := p.Complete(WithGate(ctx, "scout"), Request{}); err != nil {
		t.Fatalf("scout call: %v", err)
	}
	if _, err := p.Complete(WithGate(ctx, "critic"), Request{}); err != nil {
		t.Fatalf("critic call: %v", err)
	}
	if _, err := p.Complete(WithGate(ctx, "critic"), Request{}); err != nil {
		t.Fatalf("critic call 2: %v", err)
	}

	got := rec.Summarize()
	if len(got) != 2 {
		t.Fatalf("expected 2 gate rows, got %d: %+v", len(got), got)
	}
	if got[0].Gate != "scout" || got[0].Calls != 1 || got[0].InputTokens != 120 || got[0].OutputTokens != 45 {
		t.Errorf("scout row wrong: %+v", got[0])
	}
	if got[1].Gate != "critic" || got[1].Calls != 2 || got[1].InputTokens != 240 || got[1].OutputTokens != 90 {
		t.Errorf("critic row wrong: %+v", got[1])
	}
}

func TestTelemetryUntaggedBucketsAsUnknown(t *testing.T) {
	rec := NewRecorder()
	p := WithTelemetry(rec)(&stubProvider{
		name: "anthropic:claude",
		resp: Response{Usage: Usage{InputTokens: 1, OutputTokens: 1}},
	})
	if _, err := p.Complete(context.Background(), Request{}); err != nil {
		t.Fatalf("call: %v", err)
	}
	got := rec.Summarize()
	if len(got) != 1 || got[0].Gate != "unknown" {
		t.Fatalf("expected single unknown row, got %+v", got)
	}
}

func TestTelemetryRecordsErrors(t *testing.T) {
	rec := NewRecorder()
	p := WithTelemetry(rec)(&stubProvider{
		name: "ollama:qwen2.5-coder:14b",
		err:  errors.New("boom"),
	})
	if _, err := p.Complete(WithGate(context.Background(), "implementer"), Request{}); err == nil {
		t.Fatalf("expected error, got nil")
	}
	got := rec.Summarize()
	if len(got) != 1 || got[0].Errors != 1 || got[0].Calls != 1 {
		t.Fatalf("expected 1 error row with 1 call, got %+v", got)
	}
}

func TestNilRecorderIsNoOp(t *testing.T) {
	var rec *Recorder
	// Record on nil recorder must not panic.
	rec.Record(CallRecord{Gate: "x"})
	if got := rec.Summarize(); got != nil {
		t.Errorf("nil recorder summarize should be nil, got %+v", got)
	}
}

func TestTelemetryElapsedAccumulates(t *testing.T) {
	rec := NewRecorder()
	rec.Record(CallRecord{Gate: "architect", Elapsed: 5 * time.Second})
	rec.Record(CallRecord{Gate: "architect", Elapsed: 3 * time.Second})
	got := rec.Summarize()
	if len(got) != 1 || got[0].Elapsed != 8*time.Second {
		t.Fatalf("expected 8s elapsed, got %+v", got)
	}
}
