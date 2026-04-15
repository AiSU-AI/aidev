package llm

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Telemetry: every Complete() call that flows through a router-built
// Provider is attributed to a named pipeline gate via a context value,
// captured with token counts and elapsed time, and aggregated into a
// per-gate summary table for the headless report. The recorder is owned
// by the Router and shared across all tier providers.
//
// Attribution relies on the orchestrator tagging each agent.Run call
// with llm.WithGate(ctx, "<gate>"). Calls that arrive without a gate
// tag are bucketed as "unknown" — they still show up in the summary so
// unlabelled call sites are visible, not silently dropped.

type telemetryCtxKey int

const gateCtxKey telemetryCtxKey = 1

// WithGate tags a context so every Complete() call made through a
// router-provided Provider is attributed to the named gate.
func WithGate(ctx context.Context, gate string) context.Context {
	return context.WithValue(ctx, gateCtxKey, gate)
}

func gateFrom(ctx context.Context) string {
	if v, ok := ctx.Value(gateCtxKey).(string); ok && v != "" {
		return v
	}
	return "unknown"
}

// CallRecord is one Complete invocation captured by the telemetry
// middleware. Errors are recorded so gates that fail after retries
// still appear in the summary with their cost.
type CallRecord struct {
	Gate         string
	Provider     string
	InputTokens  int
	OutputTokens int
	Elapsed      time.Duration
	Err          error
}

// Recorder accumulates CallRecords across a run. It is safe for
// concurrent use — providers may be called from multiple goroutines
// once Architect sketch parallelism lands.
type Recorder struct {
	mu    sync.Mutex
	calls []CallRecord
}

// NewRecorder constructs an empty Recorder.
func NewRecorder() *Recorder { return &Recorder{} }

// Record appends a CallRecord. A nil Recorder is a no-op so callers
// can use telemetry optionally without nil-guarding every access.
func (r *Recorder) Record(rec CallRecord) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, rec)
}

// GateSummary aggregates calls for a single pipeline gate.
type GateSummary struct {
	Gate         string
	Provider     string
	Calls        int
	InputTokens  int
	OutputTokens int
	Elapsed      time.Duration
	Errors       int
}

// Summarize groups the recorder's calls by gate in the order each gate
// was first observed. Total row is not included; callers that want one
// can compute it from the returned slice.
func (r *Recorder) Summarize() []GateSummary {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var order []string
	seen := make(map[string]*GateSummary)
	for _, c := range r.calls {
		s, ok := seen[c.Gate]
		if !ok {
			s = &GateSummary{Gate: c.Gate}
			seen[c.Gate] = s
			order = append(order, c.Gate)
		}
		s.Provider = c.Provider
		s.Calls++
		s.InputTokens += c.InputTokens
		s.OutputTokens += c.OutputTokens
		s.Elapsed += c.Elapsed
		if c.Err != nil {
			s.Errors++
		}
	}
	out := make([]GateSummary, 0, len(order))
	for _, name := range order {
		out = append(out, *seen[name])
	}
	return out
}

// telemetryWrapper is a Provider middleware that captures tokens and
// latency per Complete call. It lives outside the retry wrapper so
// each "call" in the summary represents one logical request from the
// caller's perspective (retries are absorbed into elapsed time).
type telemetryWrapper struct {
	inner    Provider
	recorder *Recorder
}

func (t *telemetryWrapper) Name() string { return t.inner.Name() }

func (t *telemetryWrapper) Complete(ctx context.Context, req Request) (Response, error) {
	start := time.Now()
	resp, err := t.inner.Complete(ctx, req)
	t.recorder.Record(CallRecord{
		Gate:         gateFrom(ctx),
		Provider:     t.inner.Name(),
		InputTokens:  resp.Usage.InputTokens,
		OutputTokens: resp.Usage.OutputTokens,
		Elapsed:      time.Since(start),
		Err:          err,
	})
	return resp, err
}

// CompleteWithTools forwards a tool-aware request to the wrapped
// provider when it implements ToolAwareProvider and records a
// CallRecord with the same gate attribution and token/elapsed fields
// the Complete path uses. Returns ErrToolsNotSupported when the inner
// provider does not support native tool use — and does NOT record a
// CallRecord in that case, because no real work happened.
func (t *telemetryWrapper) CompleteWithTools(ctx context.Context, req ToolAwareRequest) (ToolAwareResponse, error) {
	inner, ok := t.inner.(ToolAwareProvider)
	if !ok {
		return ToolAwareResponse{}, ErrToolsNotSupported
	}
	start := time.Now()
	resp, err := inner.CompleteWithTools(ctx, req)
	// A capability error means no real LLM call happened — do not
	// emit a CallRecord for it. The inner middleware layers short-
	// circuit before touching the transport, so there's nothing to
	// attribute.
	if errors.Is(err, ErrToolsNotSupported) {
		return resp, err
	}
	t.recorder.Record(CallRecord{
		Gate:         gateFrom(ctx),
		Provider:     t.inner.Name(),
		InputTokens:  resp.Usage.InputTokens,
		OutputTokens: resp.Usage.OutputTokens,
		Elapsed:      time.Since(start),
		Err:          err,
	})
	return resp, err
}

// WithTelemetry returns a middleware that records every Complete call
// to the given recorder. Install it as the OUTERMOST layer in a
// middleware chain so retries are collapsed into a single logical call
// from the telemetry view.
func WithTelemetry(recorder *Recorder) func(Provider) Provider {
	return func(p Provider) Provider {
		return &telemetryWrapper{inner: p, recorder: recorder}
	}
}
