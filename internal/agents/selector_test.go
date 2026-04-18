package agents

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/aisu-ai/aidev/internal/github"
	"github.com/aisu-ai/aidev/internal/llm"
)

// fixedReplyProvider is a tiny llm.Provider that returns a canned
// response for every Complete call. Used to drive the LLM tie-breaker
// in tests deterministically.
type fixedReplyProvider struct {
	reply string
	err   error
	calls int
}

func (p *fixedReplyProvider) Name() string { return "test-fixed" }
func (p *fixedReplyProvider) Complete(_ context.Context, _ llm.Request) (llm.Response, error) {
	p.calls++
	if p.err != nil {
		return llm.Response{}, p.err
	}
	return llm.Response{Content: p.reply}, nil
}

func mustRubric(t *testing.T) *SketchRubric {
	t.Helper()
	r, err := LoadDefaultRubric()
	if err != nil {
		t.Fatalf("LoadDefaultRubric: %v", err)
	}
	return r
}

func makeSketch(num int, title, principleAlignment, risks, scope string) Sketch {
	body := "## Sketch " + selectorItoa(num) + ": " + title + "\n\n" +
		"### Approach\nDo a thing.\n\n" +
		"### Key decisions\n- bullet\n\n" +
		"### Trade-offs\n- pro: x\n- con: y\n\n" +
		"### Risks\n" + risks + "\n\n" +
		"### Principle alignment\n" + principleAlignment + "\n\n" +
		"### Rough scope\n" + scope + "\n"
	return Sketch{Number: num, Title: title, Markdown: body}
}

// selectorItoa is a tiny stringer for tests; avoids pulling fmt for one int.
func selectorItoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := []byte{}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

func TestParseSketchSectionsExtractsAllH3s(t *testing.T) {
	sk := makeSketch(1, "Test sketch",
		"- DRY: aligned — good\n- YAGNI: tension — okay",
		"- one risk\n- two risks",
		"3 files touched, ~50 lines.")
	got := parseSketchSections(sk.Markdown)

	for _, want := range []string{"Approach", "Key decisions", "Trade-offs", "Risks", "Principle alignment", "Rough scope"} {
		if _, ok := got[want]; !ok {
			t.Errorf("parseSketchSections missing section %q; keys: %v", want, keys(got))
		}
	}
	if !strings.Contains(got["Risks"], "one risk") {
		t.Errorf("Risks section missing body: %q", got["Risks"])
	}
}

func keys(m map[string]string) []string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestCountAlignmentSignals(t *testing.T) {
	rubric := mustRubric(t)
	cases := []struct {
		name        string
		section     string
		wantAligned int
		wantTension int
		wantViolate int
		wantDQ      bool
	}{
		{
			"all aligned",
			"- DRY: aligned — fine\n- YAGNI: aligned — fine",
			2, 0, 0, false,
		},
		{
			"mixed signals",
			"- DRY: aligned — fine\n- YAGNI: tension — okay\n- KISS: violation — bad",
			1, 1, 1, false, // KISS isn't critical
		},
		{
			"critical violation disqualifies",
			"- DRY (Rule of Three): violation — bad\n- KISS: aligned — fine",
			1, 0, 1, true,
		},
		{
			"bolded principle name still parsed",
			"- **DRY (Rule of Three)**: violation — bad",
			0, 0, 1, true,
		},
		{
			"neutral counts as nothing",
			"- DRY: neutral — no opinion\n- YAGNI: aligned — fine",
			1, 0, 0, false,
		},
		{
			"asterisk bullets work",
			"* DRY: aligned — fine",
			1, 0, 0, false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, te, v, dq := countAlignmentSignals(c.section, rubric)
			if a != c.wantAligned || te != c.wantTension || v != c.wantViolate {
				t.Errorf("counts: aligned=%d tension=%d violation=%d, want %d/%d/%d", a, te, v, c.wantAligned, c.wantTension, c.wantViolate)
			}
			if (dq != "") != c.wantDQ {
				t.Errorf("disqualify reason = %q, want non-empty=%v", dq, c.wantDQ)
			}
		})
	}
}

func TestScoreSketchHardVetoOnCriticalViolation(t *testing.T) {
	rubric := mustRubric(t)
	sk := makeSketch(1, "violates DRY",
		"- DRY (Rule of Three): violation — collapses 9 keys into 1",
		"- one risk",
		"3 files.")
	bd := scoreSketch(sk, rubric, IssueTypeOverride{})
	if !math.IsInf(bd.Score, -1) {
		t.Errorf("expected -inf for critical violation, got %v", bd.Score)
	}
	if bd.DisqualifyReason == "" {
		t.Errorf("expected DisqualifyReason, got empty")
	}
}

func TestScoreSketchSoftScoring(t *testing.T) {
	rubric := mustRubric(t)
	// 3 alignments (+3), 1 tension (-1), 0 violations, 2 risks (-1.0)
	// = +1.0 net.
	sk := makeSketch(1, "balanced",
		"- KISS: aligned — fine\n- YAGNI: aligned — fine\n- Boy Scout: aligned — fine\n- Reversibility: tension — slight",
		"- risk one\n- risk two",
		"3 files touched.")
	bd := scoreSketch(sk, rubric, IssueTypeOverride{})
	want := 1.0 // 3 + (-1) + 0 + (2 * -0.5)
	if math.Abs(bd.Score-want) > 0.001 {
		t.Errorf("score = %v, want %v", bd.Score, want)
	}
}

func TestScoreSketchRiskCapAppliesAtCeiling(t *testing.T) {
	rubric := mustRubric(t)
	// 0 alignment + 10 risks → would be -5 uncapped; risk_cap is -3.
	sk := makeSketch(1, "many risks",
		"- KISS: aligned — fine",
		strings.Repeat("- risk\n", 10),
		"3 files.")
	bd := scoreSketch(sk, rubric, IssueTypeOverride{})
	// 1 alignment + capped -3 = -2
	want := -2.0
	if math.Abs(bd.Score-want) > 0.001 {
		t.Errorf("score = %v, want %v (risk cap should hold)", bd.Score, want)
	}
}

func TestScoreSketchScopePenaltyAppliesForBugLabel(t *testing.T) {
	rubric := mustRubric(t)
	bugOverride := rubric.OverrideFor([]string{"bug"})
	// 1 alignment + 0 risks + 12 files (over 5 by 7) * 0.1 = 1 - 0.7 = 0.3
	sk := makeSketch(1, "wide bug fix",
		"- KISS: aligned — fine",
		"",
		"12 files touched.")
	bd := scoreSketch(sk, rubric, bugOverride)
	want := 0.3
	if math.Abs(bd.Score-want) > 0.001 {
		t.Errorf("score = %v, want %v (scope penalty)", bd.Score, want)
	}
}

func TestSelectorRunPicksHighestNoTieBreak(t *testing.T) {
	rubric := mustRubric(t)
	// Wide score gap → no tie-breaker should fire.
	sk1 := makeSketch(1, "weak",
		"- KISS: aligned — fine\n- YAGNI: tension — bad\n- Boy Scout: tension — bad",
		"- one risk", "3 files.")
	sk2 := makeSketch(2, "strong",
		"- KISS: aligned — fine\n- YAGNI: aligned — fine\n- Boy Scout: aligned — fine\n- DRY: aligned — fine",
		"", "3 files.")

	prov := &fixedReplyProvider{reply: ""}
	sel := &Selector{Provider: prov, Rubric: rubric}
	cc := &Context{
		Issue:    &github.Issue{Owner: "o", Repo: "r", Number: 1, Title: "t", Body: "b"},
		Sketches: []Sketch{sk1, sk2},
	}
	v, err := sel.Run(context.Background(), cc)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if v.ChosenNumber != 2 {
		t.Errorf("chosen = %d, want 2 (clear winner)", v.ChosenNumber)
	}
	if v.TieBreakerUsed {
		t.Errorf("TieBreakerUsed should be false for wide-gap pick")
	}
	if prov.calls != 0 {
		t.Errorf("provider.Complete should not be called when scores are wide; got %d calls", prov.calls)
	}
}

func TestSelectorRunInvokesTieBreakerWithinEpsilon(t *testing.T) {
	rubric := mustRubric(t)
	// Build two near-identical sketches so the deterministic
	// scores fall within epsilon and the LLM is consulted.
	sk1 := makeSketch(1, "one",
		"- KISS: aligned — fine\n- YAGNI: aligned — fine",
		"", "3 files.")
	sk2 := makeSketch(2, "two",
		"- KISS: aligned — fine\n- YAGNI: aligned — fine",
		"", "3 files.")

	prov := &fixedReplyProvider{
		reply: `{"chosen": 2, "rationale": "Sketch 2 reads better."}`,
	}
	sel := &Selector{Provider: prov, Rubric: rubric}
	cc := &Context{
		Issue:    &github.Issue{Owner: "o", Repo: "r", Number: 1, Title: "t", Body: "b"},
		Sketches: []Sketch{sk1, sk2},
	}
	v, err := sel.Run(context.Background(), cc)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !v.TieBreakerUsed {
		t.Errorf("TieBreakerUsed should be true for tied scores")
	}
	if v.ChosenNumber != 2 {
		t.Errorf("chosen = %d, want 2 (per LLM)", v.ChosenNumber)
	}
	if !strings.Contains(v.Rationale, "reads better") {
		t.Errorf("rationale missing LLM text: %q", v.Rationale)
	}
	if prov.calls != 1 {
		t.Errorf("provider.Complete should be called exactly once; got %d", prov.calls)
	}
}

func TestSelectorRunFallsBackOnTieBreakerError(t *testing.T) {
	rubric := mustRubric(t)
	sk1 := makeSketch(1, "one",
		"- KISS: aligned — fine\n- YAGNI: aligned — fine", "", "3 files.")
	sk2 := makeSketch(2, "two",
		"- KISS: aligned — fine\n- YAGNI: aligned — fine", "", "3 files.")

	prov := &fixedReplyProvider{err: errors.New("transient upstream failure")}
	sel := &Selector{Provider: prov, Rubric: rubric}
	cc := &Context{
		Issue:    &github.Issue{Owner: "o", Repo: "r", Number: 1, Title: "t", Body: "b"},
		Sketches: []Sketch{sk1, sk2},
	}
	v, err := sel.Run(context.Background(), cc)
	if err != nil {
		t.Fatalf("Run should not fail when tie-breaker errors: %v", err)
	}
	if v.ChosenNumber == 0 {
		t.Errorf("expected deterministic fallback to pick a sketch")
	}
	if v.TieBreakerUsed {
		t.Errorf("TieBreakerUsed must be false when the LLM call failed")
	}
}

func TestSelectorRunRefusesPickBelowMinImplementableScore(t *testing.T) {
	rubric := mustRubric(t)
	// Min implementable score is 0.0 (relaxed from 1.0 on 2026-04-18:
	// the Critic's `build` verdict already gated "should we build?",
	// so the Selector only refuses when a sketch is genuinely
	// net-negative — more tensions than alignments). To exercise the
	// floor we need score < 0.0: 1 aligned (+1.0) + 2 tensions (-2.0)
	// = -1.0, which is below 0.0 and should trigger refinement.
	sk := makeSketch(1, "weak",
		"- KISS: aligned — fine\n- YAGNI: tension — bad\n- DRY: tension — bad",
		"", "3 files.")

	prov := &fixedReplyProvider{}
	sel := &Selector{Provider: prov, Rubric: rubric}
	cc := &Context{
		Issue:    &github.Issue{Owner: "o", Repo: "r", Number: 1, Title: "t", Body: "b"},
		Sketches: []Sketch{sk},
	}
	v, err := sel.Run(context.Background(), cc)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if v.ChosenNumber != 0 {
		t.Errorf("expected ChosenNumber=0 for sub-min score, got %d (score %v)", v.ChosenNumber, v.Score)
	}
	if !strings.Contains(v.Rationale, "MinImplementableScore") {
		t.Errorf("rationale should explain the floor: %q", v.Rationale)
	}
}

func TestSelectorRunReportsAllDisqualified(t *testing.T) {
	rubric := mustRubric(t)
	sk1 := makeSketch(1, "veto1", "- DRY (Rule of Three): violation — bad", "", "3 files.")
	sk2 := makeSketch(2, "veto2", "- Reversibility: violation — bad", "", "3 files.")

	prov := &fixedReplyProvider{}
	sel := &Selector{Provider: prov, Rubric: rubric}
	cc := &Context{
		Issue:    &github.Issue{Owner: "o", Repo: "r", Number: 1, Title: "t", Body: "b"},
		Sketches: []Sketch{sk1, sk2},
	}
	v, err := sel.Run(context.Background(), cc)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if v.ChosenNumber != 0 {
		t.Errorf("expected no pick when all disqualified, got %d", v.ChosenNumber)
	}
	if len(v.Disqualified) != 2 {
		t.Errorf("expected 2 disqualified entries, got %v", v.Disqualified)
	}
}

func TestParseTieBreakResponseToleratesCodeFence(t *testing.T) {
	cases := []struct {
		name        string
		raw         string
		wantChosen  int
		wantInRtnle string
	}{
		{"bare json", `{"chosen": 3, "rationale": "x"}`, 3, "x"},
		{"code fence", "```json\n{\"chosen\": 1, \"rationale\": \"y\"}\n```", 1, "y"},
		{"with prose", "I'll pick: {\"chosen\": 2, \"rationale\": \"z\"}", 2, "z"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n, r, err := parseTieBreakResponse(c.raw)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if n != c.wantChosen {
				t.Errorf("chosen = %d, want %d", n, c.wantChosen)
			}
			if r != c.wantInRtnle {
				t.Errorf("rationale = %q, want %q", r, c.wantInRtnle)
			}
		})
	}
}

func TestParseFileCountFromRoughScope(t *testing.T) {
	cases := []struct {
		section string
		want    int
	}{
		{"9 files touched, ~80 lines", 9},
		{"~12 files edited", 12},
		{"3 file changed (singular)", 3},
		{"no number here", 0},
		{"", 0},
	}
	for _, c := range cases {
		got := parseFileCount(c.section)
		if got != c.want {
			t.Errorf("parseFileCount(%q) = %d, want %d", c.section, got, c.want)
		}
	}
}
