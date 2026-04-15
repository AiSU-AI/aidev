package agents

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aisu-ai/aidev/internal/github"
	"github.com/aisu-ai/aidev/internal/llm"
)

// sequencedProvider returns a canned response (or error) per call, in
// order. It fails the test if Complete is called more times than it has
// responses queued. Used to exercise the Architect's split-sketch loop.
type sequencedProvider struct {
	name    string
	replies []sequencedReply
	calls   []llm.Request
	t       *testing.T
}

type sequencedReply struct {
	body string
	err  error
}

func (s *sequencedProvider) Name() string { return s.name }
func (s *sequencedProvider) Complete(_ context.Context, req llm.Request) (llm.Response, error) {
	idx := len(s.calls)
	if idx >= len(s.replies) {
		s.t.Fatalf("sequencedProvider: call %d exceeds %d queued replies", idx+1, len(s.replies))
	}
	s.calls = append(s.calls, req)
	r := s.replies[idx]
	if r.err != nil {
		return llm.Response{}, r.err
	}
	return llm.Response{Content: r.body}, nil
}

func newArchitectCtx() *Context {
	return &Context{
		Issue: &github.Issue{
			Owner: "aisu-ai", Repo: "aidev", Number: 1,
			Title: "Test", Body: "issue body",
		},
		ScoutReport:  "scout brief body",
		CriticReport: "critic body",
	}
}

func singleSketchBody(title string) string {
	return "## Sketch 1: " + title + "\n\n### Approach\nDo a thing.\n\n### Trade-offs\n- pro: x\n\nSKETCHES: 1\n"
}

func TestParseSketchesHappyPath(t *testing.T) {
	md := `Intro paragraph the parser should skip.

## Sketch 1: Postgres-backed queue

### Approach
Use a Postgres table as the work queue.

### Trade-offs
- pro: transactional
- con: slow at 10k/s

---

## Sketch 2: Redis streams

### Approach
Redis streams for the queue.

### Trade-offs
- pro: fast
- con: extra dependency

---

## Sketch 3: SQS

### Approach
Managed AWS SQS.

### Trade-offs
- pro: zero ops
- con: vendor lock-in

SKETCHES: 3
`
	got := ParseSketches(md)
	if len(got) != 3 {
		t.Fatalf("want 3 sketches, got %d", len(got))
	}
	wantTitles := []string{"Postgres-backed queue", "Redis streams", "SQS"}
	for i, s := range got {
		if s.Number != i+1 {
			t.Errorf("sketch %d: number = %d, want %d", i, s.Number, i+1)
		}
		if s.Title != wantTitles[i] {
			t.Errorf("sketch %d: title = %q, want %q", i, s.Title, wantTitles[i])
		}
		if !strings.HasPrefix(s.Markdown, "## Sketch") {
			t.Errorf("sketch %d: body should start with H2, got %q", i, s.Markdown[:min(20, len(s.Markdown))])
		}
		if strings.Contains(s.Markdown, "SKETCHES:") {
			t.Errorf("sketch %d: body should have SKETCHES: tail stripped, got %q", i, s.Markdown)
		}
	}
}

func TestParseSketchesNoHeadersReturnsNil(t *testing.T) {
	md := "just some prose about a plan, no sketch headers at all"
	if got := ParseSketches(md); got != nil {
		t.Errorf("want nil, got %+v", got)
	}
}

func TestParseSketchesSingleSketchWithoutSeparator(t *testing.T) {
	md := `## Sketch 1: Lonely sketch

### Approach
The only one.
`
	got := ParseSketches(md)
	if len(got) != 1 {
		t.Fatalf("want 1 sketch, got %d", len(got))
	}
	if got[0].Title != "Lonely sketch" {
		t.Errorf("title = %q", got[0].Title)
	}
}

func TestParseSketchesTolerantOfWhitespaceBeforeHeader(t *testing.T) {
	md := "preamble\n\n## Sketch 1: First\n\nbody\n\n## Sketch 2: Second\n\nbody"
	got := ParseSketches(md)
	if len(got) != 2 {
		t.Fatalf("want 2 sketches, got %d", len(got))
	}
}

func TestTrimSketchesTailRemovesTrailingMarkers(t *testing.T) {
	body := "## Sketch 3: Last\n\nbody\n\n---\n\nSKETCHES: 3\n"
	got := trimSketchesTail(body)
	if strings.Contains(got, "SKETCHES:") {
		t.Errorf("SKETCHES: tail not removed: %q", got)
	}
	if strings.HasSuffix(got, "---") {
		t.Errorf("trailing --- not removed: %q", got)
	}
}

func TestEstimateCostScalesWithN(t *testing.T) {
	a := EstimateCost(1)
	b := EstimateCost(3)
	c := EstimateCost(5)
	if a.TotalOutputTokens >= b.TotalOutputTokens || b.TotalOutputTokens >= c.TotalOutputTokens {
		t.Errorf("expected monotonic growth: a=%d b=%d c=%d",
			a.TotalOutputTokens, b.TotalOutputTokens, c.TotalOutputTokens)
	}
	if a.N != 1 || b.N != 3 || c.N != 5 {
		t.Errorf("N mismatch: %d %d %d", a.N, b.N, c.N)
	}
}

func TestEstimateCostDefaultsOnZeroOrNegative(t *testing.T) {
	a := EstimateCost(0)
	b := EstimateCost(-2)
	if a.N != DefaultSketchCount || b.N != DefaultSketchCount {
		t.Errorf("want default %d, got %d and %d", DefaultSketchCount, a.N, b.N)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestArchitectRunMakesOneCallPerSketch(t *testing.T) {
	prov := &sequencedProvider{
		t:    t,
		name: "stub",
		replies: []sequencedReply{
			{body: singleSketchBody("First approach")},
			{body: singleSketchBody("Second approach")},
			{body: singleSketchBody("Third approach")},
		},
	}
	a := &Architect{Provider: prov, N: 3}

	got, err := a.Run(context.Background(), newArchitectCtx())
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if len(prov.calls) != 3 {
		t.Errorf("want 3 Complete calls, got %d", len(prov.calls))
	}
	if len(got) != 3 {
		t.Fatalf("want 3 sketches, got %d", len(got))
	}
	// Sketches must be renumbered to 1..N even though the stub returned
	// "Sketch 1" for every call.
	for i, s := range got {
		if s.Number != i+1 {
			t.Errorf("sketch %d: number = %d, want %d", i, s.Number, i+1)
		}
		wantHeader := "## Sketch " + string(rune('0'+i+1)) + ":"
		if !strings.Contains(s.Markdown, wantHeader) {
			t.Errorf("sketch %d: body missing renumbered header %q; got:\n%s", i, wantHeader, s.Markdown)
		}
	}
}

func TestArchitectRunPassesPriorTitlesAsDiversityHint(t *testing.T) {
	prov := &sequencedProvider{
		t:    t,
		name: "stub",
		replies: []sequencedReply{
			{body: singleSketchBody("Postgres queue")},
			{body: singleSketchBody("Redis streams")},
		},
	}
	a := &Architect{Provider: prov, N: 2}

	if _, err := a.Run(context.Background(), newArchitectCtx()); err != nil {
		t.Fatalf("Run error: %v", err)
	}
	// The second call's system prompt must mention the first sketch's title.
	if !strings.Contains(prov.calls[1].System, "Postgres queue") {
		t.Errorf("second call system prompt missing prior title 'Postgres queue':\n%s", prov.calls[1].System)
	}
	// The first call must NOT mention the second sketch (obviously, it hasn't happened yet).
	if strings.Contains(prov.calls[0].System, "Redis streams") {
		t.Errorf("first call should not reference later sketches")
	}
	// The first call should not have the PRIOR SKETCHES section at all.
	if strings.Contains(prov.calls[0].System, "PRIOR SKETCHES") {
		t.Errorf("first call should not have PRIOR SKETCHES section")
	}
}

func TestArchitectRunReturnsPartialSuccessOnMidRunFailure(t *testing.T) {
	prov := &sequencedProvider{
		t:    t,
		name: "stub",
		replies: []sequencedReply{
			{body: singleSketchBody("First good")},
			{err: errors.New("transient upstream failure")},
			{body: singleSketchBody("Third good")},
		},
	}
	a := &Architect{Provider: prov, N: 3}

	got, err := a.Run(context.Background(), newArchitectCtx())
	if err != nil {
		t.Fatalf("Run should tolerate partial failure, got err: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 surviving sketches, got %d", len(got))
	}
	// Renumbering uses the loop index so gaps collapse to 1..2 in the
	// returned slice but the sketches themselves carry their original
	// run-index numbers (1 and 3). That's fine — ParseSketches in the
	// reporter re-renders the list in order.
	if got[0].Number != 1 || got[1].Number != 3 {
		t.Errorf("partial-success numbering: got %d and %d, want 1 and 3", got[0].Number, got[1].Number)
	}
}

func TestArchitectRunFailsWhenAllCallsFail(t *testing.T) {
	prov := &sequencedProvider{
		t:    t,
		name: "stub",
		replies: []sequencedReply{
			{err: errors.New("boom 1")},
			{err: errors.New("boom 2")},
		},
	}
	a := &Architect{Provider: prov, N: 2}

	_, err := a.Run(context.Background(), newArchitectCtx())
	if err == nil {
		t.Fatal("expected error when all sketch calls fail, got nil")
	}
	if !strings.Contains(err.Error(), "boom 1") {
		t.Errorf("error should surface first failure, got: %v", err)
	}
}

func TestRenumberSketchRewritesHeaderOnly(t *testing.T) {
	in := "## Sketch 1: My Title\n\nbody text\n\n## Sketch 1: Nested ref"
	got := renumberSketch(in, 4)
	if !strings.Contains(got, "## Sketch 4: My Title") {
		t.Errorf("renumbered header missing: %q", got)
	}
	if strings.Contains(got, "## Sketch 1:") {
		t.Errorf("old number 1 still present: %q", got)
	}
	if !strings.Contains(got, "body text") {
		t.Errorf("body text lost: %q", got)
	}
}
