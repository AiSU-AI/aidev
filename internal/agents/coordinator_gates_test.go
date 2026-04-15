package agents

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aisu-ai/aidev/internal/github"
	"github.com/aisu-ai/aidev/internal/llm"
	"github.com/aisu-ai/aidev/internal/repo"
)

// ---------------------------------------------------------------------------
// parseObserveResponse — the OK:/NOTE: grammar parser
// ---------------------------------------------------------------------------

func TestParseObserveResponseOK(t *testing.T) {
	got, err := parseObserveResponse("post-scout", "OK: brief covers all locales mentioned in the issue.\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d notes, want 1", len(got))
	}
	if got[0].Severity != "info" {
		t.Errorf("OK should produce info severity, got %q", got[0].Severity)
	}
	if !strings.Contains(got[0].Body, "brief covers all locales") {
		t.Errorf("body lost the OK summary, got %q", got[0].Body)
	}
}

func TestParseObserveResponseBareOK(t *testing.T) {
	got, err := parseObserveResponse("post-critic", "OK")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Severity != "info" {
		t.Errorf("bare OK should produce one info note, got %+v", got)
	}
}

func TestParseObserveResponseNoteWithSeverity(t *testing.T) {
	in := "NOTE: warn | the architect's sketches 1 and 2 are functionally identical.\n\n- Sketch 1: rename + delete\n- Sketch 2: rename + delete (with a different commit message)\n"
	got, err := parseObserveResponse("post-architect", in)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d notes, want 1", len(got))
	}
	if got[0].Severity != "warn" {
		t.Errorf("severity = %q, want warn", got[0].Severity)
	}
	if !strings.Contains(got[0].Body, "functionally identical") {
		t.Errorf("body missing summary, got %q", got[0].Body)
	}
	if !strings.Contains(got[0].Body, "Sketch 1") {
		t.Errorf("body missing bullets, got %q", got[0].Body)
	}
}

func TestParseObserveResponseConcernSeverity(t *testing.T) {
	got, err := parseObserveResponse("post-reviewer", "NOTE: concern | Reviewer missed dangling refs to deleted key.\n\n- pricing.tsx still calls t('contactSales')")
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Severity != "concern" {
		t.Errorf("severity = %q, want concern", got[0].Severity)
	}
}

func TestParseObserveResponseUnknownSeverityFallsBackToInfo(t *testing.T) {
	got, err := parseObserveResponse("post-scout", "NOTE: critical | bad\n\n- thing")
	if err != nil {
		t.Fatal(err)
	}
	// "critical" is not in {info, warn, concern} so the parser
	// falls back to info rather than rejecting — we'd rather
	// surface a possibly-mis-typed note than swallow it.
	if got[0].Severity != "info" {
		t.Errorf("unknown severity should fall back to info, got %q", got[0].Severity)
	}
}

func TestParseObserveResponseRejectsUnknownVerdict(t *testing.T) {
	_, err := parseObserveResponse("post-scout", "MAYBE: who knows\n\n- thing")
	if err == nil {
		t.Error("expected error on unknown verdict")
	}
}

func TestParseObserveResponseRejectsEmpty(t *testing.T) {
	_, err := parseObserveResponse("post-scout", "")
	if err == nil {
		t.Error("expected error on empty")
	}
}

func TestParseObserveResponseStripsCodeFence(t *testing.T) {
	in := "```\nOK: looks fine\n```"
	got, err := parseObserveResponse("post-scout", in)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Severity != "info" {
		t.Errorf("fenced OK should still parse, got %+v", got)
	}
}

// ---------------------------------------------------------------------------
// runObserveGate — the shared LLM call wrapper
// ---------------------------------------------------------------------------

func TestRunObserveGateAppendsNotesToCoordinator(t *testing.T) {
	co := &Coordinator{
		Provider: &capturingProvider{
			reply: "NOTE: warn | brief is missing the i18n directory.\n\n- apps/marketing has 9 locale files but the Scout brief only mentions 3",
		},
	}
	notes, err := co.runObserveGate(context.Background(), "post-scout", "(test prompt)")
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 {
		t.Fatalf("got %d notes, want 1", len(notes))
	}
	if len(co.Notes) != 1 {
		t.Fatalf("Coordinator.Notes should have grown by 1, got %d", len(co.Notes))
	}
	if co.Notes[0].Gate != "post-scout" {
		t.Errorf("note Gate = %q", co.Notes[0].Gate)
	}
}

func TestRunObserveGateAccumulatesAcrossCalls(t *testing.T) {
	provider := &scriptedProvider{
		responses: []string{
			"OK: scout brief is fine",
			"NOTE: warn | critic reused a clarifier-answered question",
		},
	}
	co := &Coordinator{Provider: provider}
	if _, err := co.runObserveGate(context.Background(), "post-scout", "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := co.runObserveGate(context.Background(), "post-critic", "x"); err != nil {
		t.Fatal(err)
	}
	if len(co.Notes) != 2 {
		t.Errorf("notes accumulated = %d, want 2", len(co.Notes))
	}
	if co.Notes[0].Gate != "post-scout" || co.Notes[1].Gate != "post-critic" {
		t.Errorf("note order/gates wrong: %+v", co.Notes)
	}
}

// ---------------------------------------------------------------------------
// Per-gate happy paths — verify each gate produces a reasonable
// prompt for its position in the pipeline. We use a capturing
// provider to inspect the prompt text.
// ---------------------------------------------------------------------------

func TestObserveScoutBriefPromptShape(t *testing.T) {
	p := &capturingProvider{reply: "OK: looks fine"}
	co := &Coordinator{Provider: p}
	cc := &Context{
		Issue:       &github.Issue{Owner: "a", Repo: "b", Number: 1, Title: "T", Body: "issue body"},
		Snapshot:    &repo.Snapshot{Root: "/tmp"},
		ScoutReport: "## Purpose\nA test repo.\n\n## Architecture\n- internal/foo",
	}
	if _, err := co.ObserveScoutBrief(context.Background(), cc); err != nil {
		t.Fatal(err)
	}
	body := p.req.Messages[0].Content
	for _, want := range []string{
		"# Gate: post-Scout",
		"## Issue a/b#1",
		"## Scout brief",
		"## What to look for at this gate",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("post-scout prompt missing %q; got:\n%s", want, body)
		}
	}
}

func TestObserveScoutBriefRejectsMissingScoutReport(t *testing.T) {
	co := &Coordinator{Provider: &capturingProvider{}}
	cc := &Context{
		Issue: &github.Issue{Owner: "a", Repo: "b", Number: 1, Title: "T", Body: "b"},
	}
	if _, err := co.ObserveScoutBrief(context.Background(), cc); err == nil {
		t.Error("expected error when ScoutReport is empty")
	}
}

func TestObserveCriticReportPromptIncludesClarifierWhenPresent(t *testing.T) {
	p := &capturingProvider{reply: "OK: consistent"}
	co := &Coordinator{Provider: p}
	cc := &Context{
		Issue:          &github.Issue{Owner: "a", Repo: "b", Number: 1, Title: "T", Body: "issue body"},
		CriticReport:   "## Restatement\n...\nRECOMMENDATION: build",
		ClarifierNotes: "- **Q (q1):** is it a button? **A:** yes",
	}
	if _, err := co.ObserveCriticReport(context.Background(), cc); err != nil {
		t.Fatal(err)
	}
	body := p.req.Messages[0].Content
	if !strings.Contains(body, "## Critic report") {
		t.Errorf("missing critic report section")
	}
	if !strings.Contains(body, "Clarifier session") {
		t.Errorf("clarifier section should be included when ClarifierNotes is set; got:\n%s", body)
	}
}

func TestObserveCriticReportSkipsClarifierSectionWhenAbsent(t *testing.T) {
	p := &capturingProvider{reply: "OK: ok"}
	co := &Coordinator{Provider: p}
	cc := &Context{
		Issue:        &github.Issue{Owner: "a", Repo: "b", Number: 1, Title: "T", Body: "b"},
		CriticReport: "RECOMMENDATION: build",
	}
	if _, err := co.ObserveCriticReport(context.Background(), cc); err != nil {
		t.Fatal(err)
	}
	// The "what to look for" instructions reference "Clarifier
	// session" generically; what we actually want to verify is
	// that the dedicated "## Clarifier session (already collected)"
	// section heading does NOT appear when ClarifierNotes is
	// empty — that section would otherwise carry a misleading
	// empty body.
	if strings.Contains(p.req.Messages[0].Content, "## Clarifier session (already collected)") {
		t.Error("clarifier section heading should NOT appear when ClarifierNotes is empty")
	}
}

func TestObserveArchitectSketchesPromptIncludesEverySketch(t *testing.T) {
	p := &capturingProvider{reply: "OK: distinct"}
	co := &Coordinator{Provider: p}
	cc := &Context{
		Issue:        &github.Issue{Owner: "a", Repo: "b", Number: 1, Title: "T", Body: "b"},
		CriticReport: "RECOMMENDATION: build",
		Sketches: []Sketch{
			{Number: 1, Title: "manual rename", Markdown: "## Plan\nRename in place"},
			{Number: 2, Title: "namespaced consolidation", Markdown: "## Plan\nNew namespace"},
			{Number: 3, Title: "shared component", Markdown: "## Plan\nExtract a button"},
		},
	}
	if _, err := co.ObserveArchitectSketches(context.Background(), cc); err != nil {
		t.Fatal(err)
	}
	body := p.req.Messages[0].Content
	for _, want := range []string{
		"### Sketch 1: manual rename",
		"### Sketch 2: namespaced consolidation",
		"### Sketch 3: shared component",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("post-architect prompt missing %q", want)
		}
	}
}

func TestObserveArchitectSketchesRejectsEmptySketchSet(t *testing.T) {
	co := &Coordinator{Provider: &capturingProvider{}}
	cc := &Context{
		Issue: &github.Issue{Owner: "a", Repo: "b", Number: 1, Title: "T", Body: "b"},
	}
	if _, err := co.ObserveArchitectSketches(context.Background(), cc); err == nil {
		t.Error("expected error on empty sketches")
	}
}

func TestObserveReviewerVerdictPromptIncludesRunningNotes(t *testing.T) {
	p := &capturingProvider{reply: "OK: reviewer caught what we caught"}
	co := &Coordinator{
		Provider: p,
		// Pre-populate notes from "earlier gates" in this run.
		Notes: []GateNote{
			{Gate: "post-scout", Severity: "warn", Body: "brief missing locale dir"},
			{Gate: "post-architect", Severity: "concern", Body: "sketch 1 and 2 are identical"},
		},
	}
	cc := &Context{
		Issue: &github.Issue{Owner: "a", Repo: "b", Number: 1, Title: "T", Body: "b"},
	}
	rev := &Review{
		Markdown: "approved",
		Verdict:  "approve",
	}
	if _, err := co.ObserveReviewerVerdict(context.Background(), cc, rev); err != nil {
		t.Fatal(err)
	}
	body := p.req.Messages[0].Content
	if !strings.Contains(body, "Earlier monitor notes") {
		t.Errorf("post-reviewer prompt should include earlier notes; got:\n%s", body)
	}
	if !strings.Contains(body, "brief missing locale dir") {
		t.Errorf("post-reviewer prompt should quote the post-scout note")
	}
	if !strings.Contains(body, "[post-architect/concern]") {
		t.Errorf("post-reviewer prompt should label notes by gate and severity")
	}
}

func TestObserveReviewerVerdictWithoutEarlierNotes(t *testing.T) {
	p := &capturingProvider{reply: "OK: clean"}
	co := &Coordinator{Provider: p}
	cc := &Context{
		Issue: &github.Issue{Owner: "a", Repo: "b", Number: 1, Title: "T", Body: "b"},
	}
	if _, err := co.ObserveReviewerVerdict(context.Background(), cc, &Review{Verdict: "approve"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(p.req.Messages[0].Content, "Earlier monitor notes") {
		t.Error("post-reviewer prompt should NOT include the earlier-notes section when there are no notes")
	}
}

// ---------------------------------------------------------------------------
// Provider error path — every gate must fail safely (return error
// from runObserveGate) so the orchestrator's runAdvisoryGate
// wrapper can swallow it without blocking the pipeline.
// ---------------------------------------------------------------------------

func TestObserveGateProviderErrorIsPropagated(t *testing.T) {
	co := &Coordinator{Provider: &erroringProvider{}}
	cc := &Context{
		Issue:       &github.Issue{Owner: "a", Repo: "b", Number: 1, Title: "T", Body: "b"},
		ScoutReport: "brief",
	}
	_, err := co.ObserveScoutBrief(context.Background(), cc)
	if err == nil {
		t.Fatal("expected provider error to surface to caller")
	}
	// Notes must NOT be populated on a failed call.
	if len(co.Notes) != 0 {
		t.Errorf("Notes should be empty on provider error, got %d", len(co.Notes))
	}
}

// erroringProvider returns an error on every Complete call,
// simulating a rate limit or network drop. Used to prove that
// the gate methods propagate provider failures cleanly so the
// orchestrator's runAdvisoryGate wrapper can swallow them
// without ever populating Coordinator.Notes.
type erroringProvider struct{}

func (erroringProvider) Name() string { return "erroring" }
func (erroringProvider) Complete(_ context.Context, _ llm.Request) (llm.Response, error) {
	return llm.Response{}, errors.New("simulated rate limit")
}
