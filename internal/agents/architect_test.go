package agents

import (
	"strings"
	"testing"
)

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
