package agents

import (
	"context"
	"strings"
	"testing"

	"github.com/aisu-ai/aidev/internal/github"
)

func TestParseCoordinatorReviewApproved(t *testing.T) {
	raw := "APPROVED: diff correctly migrates the 9 locale files and imports.\n"
	got, err := parseCoordinatorReview(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Approved {
		t.Errorf("want Approved, got %+v", got)
	}
	if !strings.Contains(got.Rationale, "9 locale files") {
		t.Errorf("rationale = %q", got.Rationale)
	}
	if got.Feedback != "" {
		t.Errorf("approved review should not have feedback; got %q", got.Feedback)
	}
}

func TestParseCoordinatorReviewConcerns(t *testing.T) {
	raw := "CONCERNS: German and French values are fabricated.\n\n- locales/de/common.json: \"Kontakt Vertrieb\" is invented; request the real value via NEED_FILES\n- locales/fr/common.json: same issue\n"
	got, err := parseCoordinatorReview(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.Approved {
		t.Errorf("expected not Approved, got %+v", got)
	}
	if !strings.Contains(got.Rationale, "fabricated") {
		t.Errorf("rationale = %q", got.Rationale)
	}
	if !strings.Contains(got.Feedback, "Kontakt Vertrieb") {
		t.Errorf("feedback missing de bullet; got:\n%s", got.Feedback)
	}
	if !strings.Contains(got.Feedback, "locales/fr") {
		t.Errorf("feedback missing fr bullet; got:\n%s", got.Feedback)
	}
}

func TestParseCoordinatorReviewStripsLeadingFence(t *testing.T) {
	raw := "```\nAPPROVED: all good.\n```\n"
	got, err := parseCoordinatorReview(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Approved {
		t.Error("fence-stripped approved response should still parse")
	}
}

func TestParseCoordinatorReviewRejectsUnknownVerdict(t *testing.T) {
	_, err := parseCoordinatorReview("MAYBE: who knows\n")
	if err == nil {
		t.Error("expected error on unknown verdict")
	}
}

func TestParseCoordinatorReviewRejectsEmptyConcernsBody(t *testing.T) {
	_, err := parseCoordinatorReview("CONCERNS: things are bad\n")
	if err == nil {
		t.Error("expected error on CONCERNS without a body")
	}
}

func TestParseCoordinatorReviewRejectsEmpty(t *testing.T) {
	_, err := parseCoordinatorReview("")
	if err == nil {
		t.Error("expected error on empty")
	}
	_, err = parseCoordinatorReview("   \n  \n")
	if err == nil {
		t.Error("expected error on whitespace-only")
	}
}

// TestCoordinatorReviewPromptIncludesDiff drives an end-to-end
// Review call with a capturing provider to verify the prompt the
// model receives includes the diff body under a review heading and
// echoes the issue/sketch context. This is the regression guard
// against a refactor that accidentally drops the diff from the
// prompt — a subtle failure mode where the Coordinator would
// "review" without actually seeing the code.
func TestCoordinatorReviewPromptIncludesDiff(t *testing.T) {
	p := &capturingProvider{reply: "APPROVED: minimal diff, clean change.\n"}
	co := &Coordinator{Provider: p}
	cc := &Context{
		Issue: &github.Issue{
			Owner: "aisu-ai", Repo: "aidev", Number: 1,
			Title: "T", Body: "issue body",
		},
		ScoutReport:  "scout brief body",
		CriticReport: "critic body",
	}
	sketch := &Sketch{Number: 1, Title: "t", Markdown: "## Plan\n- x"}
	patch := &Patch{Diff: "diff --git a/x.go b/x.go\n--- a/x.go\n+++ b/x.go\n@@ -1 +1 @@\n-foo\n+bar\n"}

	review, err := co.Review(context.Background(), cc, sketch, patch)
	if err != nil {
		t.Fatal(err)
	}
	if !review.Approved {
		t.Error("expected approval from canned response")
	}
	body := p.req.Messages[0].Content
	for _, want := range []string{
		"## Implementer diff under review",
		"diff --git a/x.go b/x.go",
		"bar",
		"## Scout brief",
		"## Chosen sketch 1",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("prompt missing %q; got:\n%s", want, body)
		}
	}
}

func TestCoordinatorRejectsEmptyPatch(t *testing.T) {
	co := &Coordinator{Provider: &capturingProvider{}}
	cc := &Context{
		Issue: &github.Issue{Owner: "a", Repo: "b", Number: 1, Title: "T", Body: "b"},
	}
	sketch := &Sketch{Number: 1, Title: "t", Markdown: "m"}
	_, err := co.Review(context.Background(), cc, sketch, &Patch{Diff: "   "})
	if err == nil {
		t.Error("expected error on whitespace-only diff")
	}
}
