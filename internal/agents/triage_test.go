package agents

import (
	"context"
	"strings"
	"testing"

	"github.com/aisu-ai/aidev/internal/github"
)

// triageContext builds a Context with one chosen Sketch (number 1) so
// Triage tests don't need to repeat the wiring for each case.
func triageContext(t *testing.T, sketchAlignment string, sketchExtras ...string) *Context {
	t.Helper()
	body := "## Sketch 1: ContactSales unification\n\n" +
		"### Approach\nUnify all sales-CTA i18n keys.\n\n" +
		"### Key decisions\n" +
		"- No contactSalesTeam sibling key UNLESS the call site is body copy that requires the longer phrasing.\n" +
		"- All other sites collapse to common.cta.contactSales.\n\n" +
		"### Principle alignment\n" + sketchAlignment + "\n\n" +
		"### Rough scope\n9 locale files, ~10 TSX call sites.\n"
	for _, extra := range sketchExtras {
		body += "\n" + extra
	}
	return &Context{
		Issue:    &github.Issue{Owner: "AiSU-AI", Repo: "Company-Site", Number: 533, Title: "test", Body: ""},
		Sketches: []Sketch{{Number: 1, Title: "ContactSales unification", Markdown: body}},
		Selector: &SelectorVerdict{ChosenNumber: 1, Score: 1.5, Rationale: "test"},
	}
}

func triageReview(verdict string, blockers []string) *Review {
	r := &Review{Verdict: verdict, Markdown: "test review markdown"}
	for _, b := range blockers {
		r.Blockers = append(r.Blockers, ReviewNote{Severity: "blocker", Text: b})
	}
	return r
}

// runTriageWithLLM drives the Triage agent end-to-end with a canned
// LLM JSON reply so we exercise both the LLM-output parsing and the
// Go-side coercions in one flow.
func runTriageWithLLM(t *testing.T, llmReply string, in TriageInput, c *Context) *TriageVerdict {
	t.Helper()
	prov := &fixedReplyProvider{reply: llmReply}
	tri := &Triage{Provider: prov}
	v, err := tri.Run(context.Background(), c, in)
	if err != nil {
		t.Fatalf("Triage.Run: %v", err)
	}
	return v
}

func TestTriageHappyPathFixNow(t *testing.T) {
	c := triageContext(t, "- DRY: aligned\n- KISS: aligned")
	llm := `{"actions": [
		{"id":"f1","source":"reviewer","severity":"blocker","finding":"Missing test for X",
		 "action":"fix_now","rationale":"Add the missing test","fix_plan":"Add test/x.spec.ts"}
	]}`
	in := TriageInput{Review: triageReview("changes_requested", []string{"Missing test"}), Patch: "diff", Round: 1}
	v := runTriageWithLLM(t, llm, in, c)
	if v.Convergence != "continue" {
		t.Errorf("expected convergence=continue, got %q", v.Convergence)
	}
	if len(v.Actions) != 1 || v.Actions[0].Action != "fix_now" {
		t.Errorf("expected one fix_now action, got %+v", v.Actions)
	}
	if len(v.CoercionsLog) != 0 {
		t.Errorf("no coercions expected, got %v", v.CoercionsLog)
	}
}

func TestTriageGroundedRebutPasses(t *testing.T) {
	c := triageContext(t, "- DRY: aligned")
	citation := "No contactSalesTeam sibling key UNLESS the call site is body copy"
	llm := `{"actions": [
		{"id":"f1","source":"reviewer","severity":"blocker",
		 "finding":"DRY violated: contactSalesTeam used once",
		 "action":"rebut","rationale":"Sketch explicitly allows it",
		 "rebut_text":"The Sketch sanctions the sibling key for prose context.",
		 "sketch_citation":"` + citation + `"}
	]}`
	in := TriageInput{Review: triageReview("changes_requested", []string{"DRY violated"}), Patch: "diff", Round: 1}
	v := runTriageWithLLM(t, llm, in, c)
	if v.Convergence != "rebut_to_ship" {
		t.Errorf("expected convergence=rebut_to_ship, got %q", v.Convergence)
	}
	if v.Actions[0].Action != "rebut" {
		t.Errorf("rebut should be preserved when citation matches, got %q", v.Actions[0].Action)
	}
	if len(v.CoercionsLog) != 0 {
		t.Errorf("no coercions expected, got %v", v.CoercionsLog)
	}
}

func TestTriageRebutWithoutCitationCoercedToEscalate(t *testing.T) {
	c := triageContext(t, "- DRY: aligned")
	llm := `{"actions": [
		{"id":"f1","source":"reviewer","severity":"blocker",
		 "finding":"Naming convention violated",
		 "action":"rebut","rationale":"Reviewer is wrong",
		 "rebut_text":"I disagree."}
	]}`
	in := TriageInput{Review: triageReview("changes_requested", []string{"Naming"}), Patch: "diff", Round: 1}
	v := runTriageWithLLM(t, llm, in, c)
	if v.Actions[0].Action != "escalate" {
		t.Errorf("ungrounded rebut must coerce to escalate, got %q", v.Actions[0].Action)
	}
	if v.Convergence != "escalate" {
		t.Errorf("convergence should be escalate, got %q", v.Convergence)
	}
	if len(v.CoercionsLog) == 0 || !strings.Contains(v.CoercionsLog[0], "ungrounded citation") {
		t.Errorf("expected coercion log entry, got %v", v.CoercionsLog)
	}
}

func TestTriageRebutWithInventedCitationCoercedToEscalate(t *testing.T) {
	c := triageContext(t, "- DRY: aligned")
	llm := `{"actions": [
		{"id":"f1","source":"reviewer","severity":"blocker",
		 "finding":"Missing guardrail",
		 "action":"rebut","rationale":"Sketch says so",
		 "rebut_text":"The Sketch covers it.",
		 "sketch_citation":"This phrase does not appear anywhere in the sketch markdown"}
	]}`
	in := TriageInput{Review: triageReview("changes_requested", []string{"Guardrail"}), Patch: "diff", Round: 1}
	v := runTriageWithLLM(t, llm, in, c)
	if v.Actions[0].Action != "escalate" {
		t.Errorf("invented citation must coerce to escalate, got %q", v.Actions[0].Action)
	}
}

func TestTriageProtectedPathFixCoercedToEscalate(t *testing.T) {
	c := triageContext(t, "- DRY: aligned")
	llm := `{"actions": [
		{"id":"f1","source":"reviewer","severity":"blocker",
		 "finding":"Schema needs index","action":"fix_now",
		 "rationale":"Add index","fix_plan":"Edit apps/api/migrations/0042_add_index.sql to add CREATE INDEX"}
	]}`
	in := TriageInput{Review: triageReview("changes_requested", []string{"Index"}), Patch: "diff", Round: 1}
	v := runTriageWithLLM(t, llm, in, c)
	if v.Actions[0].Action != "escalate" {
		t.Errorf("protected-path fix_plan must coerce to escalate, got %q", v.Actions[0].Action)
	}
	if !strings.Contains(strings.Join(v.CoercionsLog, " "), "protected path") {
		t.Errorf("expected protected-path coercion log, got %v", v.CoercionsLog)
	}
}

func TestTriageSecurityCIRebutCoercedToEscalate(t *testing.T) {
	c := triageContext(t, "- DRY: aligned")
	citation := "No contactSalesTeam sibling key UNLESS the call site is body copy"
	llm := `{"actions": [
		{"id":"ci-1","source":"ci","ci_check":"CodeQL","severity":"blocker",
		 "finding":"js/sql-injection at app.ts:42","action":"rebut",
		 "rationale":"False positive","rebut_text":"Not exploitable",
		 "sketch_citation":"` + citation + `"}
	]}`
	in := TriageInput{Review: triageReview("changes_requested", nil), Patch: "diff", Round: 1}
	v := runTriageWithLLM(t, llm, in, c)
	if v.Actions[0].Action != "escalate" {
		t.Errorf("security CI rebut must coerce to escalate (citation OR not), got %q", v.Actions[0].Action)
	}
	if !strings.Contains(strings.Join(v.CoercionsLog, " "), "ci-security") {
		t.Errorf("expected security coercion log, got %v", v.CoercionsLog)
	}
}

func TestTriageCycleProtectionForcesEscalate(t *testing.T) {
	c := triageContext(t, "- DRY: aligned")
	llm := `{"actions": [
		{"id":"f1","source":"reviewer","severity":"blocker",
		 "finding":"Lint error","action":"fix_now",
		 "rationale":"Run eslint --fix","fix_plan":"Edit src/foo.ts line 12"}
	]}`
	prev := []TriageAction{{ID: "f1", Action: "fix_now"}}
	in := TriageInput{Review: triageReview("changes_requested", []string{"Lint"}), Patch: "diff", Round: 2, PrevActions: prev}
	v := runTriageWithLLM(t, llm, in, c)
	if v.Actions[0].Action != "escalate" {
		t.Errorf("cycle-protected fix_now must coerce to escalate, got %q", v.Actions[0].Action)
	}
	if !strings.Contains(strings.Join(v.CoercionsLog, " "), "cycle protection") {
		t.Errorf("expected cycle-protection log, got %v", v.CoercionsLog)
	}
}

func TestTriageConvergenceApproveOnEmptyActions(t *testing.T) {
	c := triageContext(t, "- DRY: aligned")
	llm := `{"actions": []}`
	in := TriageInput{Review: triageReview("approve", nil), Patch: "diff", Round: 1}
	v := runTriageWithLLM(t, llm, in, c)
	if v.Convergence != "approve" {
		t.Errorf("expected convergence=approve when no actions, got %q", v.Convergence)
	}
}

func TestTriageConvergenceContinueDominatesRebut(t *testing.T) {
	c := triageContext(t, "- DRY: aligned")
	citation := "No contactSalesTeam sibling key UNLESS the call site is body copy"
	llm := `{"actions": [
		{"id":"f1","source":"reviewer","severity":"blocker","finding":"X","action":"fix_now","rationale":"r","fix_plan":"Edit src/a.ts line 5"},
		{"id":"f2","source":"reviewer","severity":"suggestion","finding":"Y","action":"rebut","rationale":"r","rebut_text":"t","sketch_citation":"` + citation + `"}
	]}`
	in := TriageInput{Review: triageReview("changes_requested", nil), Patch: "diff", Round: 1}
	v := runTriageWithLLM(t, llm, in, c)
	if v.Convergence != "continue" {
		t.Errorf("expected convergence=continue (fix_now dominates), got %q", v.Convergence)
	}
}

func TestTriageMalformedJSONReturnsEscalate(t *testing.T) {
	c := triageContext(t, "- DRY: aligned")
	llm := `not json at all`
	in := TriageInput{Review: triageReview("changes_requested", nil), Patch: "diff", Round: 1}
	v := runTriageWithLLM(t, llm, in, c)
	if v.Convergence != "escalate" {
		t.Errorf("malformed JSON must converge to escalate, got %q", v.Convergence)
	}
	if !strings.Contains(v.EscalateReason, "could not parse") {
		t.Errorf("escalate reason should mention parse failure, got %q", v.EscalateReason)
	}
}

func TestTriageStripsCodeFences(t *testing.T) {
	c := triageContext(t, "- DRY: aligned")
	llm := "```json\n{\"actions\": [{\"id\":\"f1\",\"source\":\"reviewer\",\"severity\":\"blocker\",\"finding\":\"X\",\"action\":\"fix_now\",\"rationale\":\"r\",\"fix_plan\":\"Edit src/a.ts line 5\"}]}\n```"
	in := TriageInput{Review: triageReview("changes_requested", nil), Patch: "diff", Round: 1}
	v := runTriageWithLLM(t, llm, in, c)
	if v.Convergence != "continue" {
		t.Errorf("fenced JSON should still parse, got convergence=%q", v.Convergence)
	}
}

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pattern, candidate string
		want               bool
	}{
		{"**/migrations/**", "apps/api/migrations/0042.sql", true},
		{"**/migrations/**", "src/components/foo.ts", false},
		{"*.sql", "schema.sql", true},
		{"*.sql", "apps/db/schema.sql", false}, // *.sql doesn't cross /
		{"**/.github/workflows/**", ".github/workflows/ci.yml", true},
		{"**/secrets/**", "config/secrets/prod.key", true},
		{"*.env", ".env", true},
		{"*.env.*", ".env.production", true},
	}
	for _, c := range cases {
		t.Run(c.pattern+"_"+c.candidate, func(t *testing.T) {
			if got := globMatch(c.pattern, c.candidate); got != c.want {
				t.Errorf("globMatch(%q, %q) = %v, want %v", c.pattern, c.candidate, got, c.want)
			}
		})
	}
}

func TestSketchContainsCitation(t *testing.T) {
	sketch := "## Sketch 1\n### Key decisions\n- No contactSalesTeam sibling key UNLESS the call site is body copy.\n"
	cases := []struct {
		citation string
		want     bool
	}{
		{"No contactSalesTeam sibling key UNLESS the call site is body copy", true},
		{"NO   CONTACTSALESTEAM   SIBLING   KEY   UNLESS", true}, // case + whitespace tolerance
		{"completely fabricated quote that is not in the sketch", false},
		{"short", false}, // below minCitationLen
		{"", false},
	}
	for _, c := range cases {
		t.Run(c.citation, func(t *testing.T) {
			if got := sketchContainsCitation(sketch, c.citation); got != c.want {
				t.Errorf("sketchContainsCitation(_, %q) = %v, want %v", c.citation, got, c.want)
			}
		})
	}
}

func TestIsSecurityCheck(t *testing.T) {
	yes := []string{"CodeQL", "Snyk", "Dependabot Alerts", "Trivy scan", "Semgrep", "npm audit", "ossf scorecard"}
	no := []string{"build", "test", "lint", "type-check", "unit-tests"}
	for _, n := range yes {
		if !isSecurityCheck(n) {
			t.Errorf("isSecurityCheck(%q) should be true", n)
		}
	}
	for _, n := range no {
		if isSecurityCheck(n) {
			t.Errorf("isSecurityCheck(%q) should be false", n)
		}
	}
}
