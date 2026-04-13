package agents

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sampleReview = `# Review

## Blockers
- Uses ` + "`" + `any` + "`" + ` type in internal/foo.go:42 — violates "No any types" principle
- Missing test for the edge case when x == 0 in internal/bar.go

## Suggestions
- Extract the anonymous function in baz.go:17 to a named helper for readability
- Add a doc comment on the new ExportedFunc

## Follow-up issues
- **Refactor oldPkg/legacy.go** — 200-line function the new code touches indirectly (labels: tech-debt, refactor)
- **Add integration test for the Foo flow** — no existing coverage; the new code nearly touches it (labels: testing)
- summary paragraph that should not become an issue

## Verdict

VERDICT: changes_requested
`

func TestParseReviewNotes(t *testing.T) {
	blockers := parseReviewNotes(sampleReview, "Blockers", "blocker")
	if len(blockers) != 2 {
		t.Errorf("got %d blockers, want 2", len(blockers))
	}
	for _, b := range blockers {
		if b.Severity != "blocker" {
			t.Errorf("severity = %q, want blocker", b.Severity)
		}
	}

	suggests := parseReviewNotes(sampleReview, "Suggestions", "suggestion")
	if len(suggests) != 2 {
		t.Errorf("got %d suggestions, want 2", len(suggests))
	}
}

func TestParseReviewNotesEmptySectionReturnsNil(t *testing.T) {
	md := "# Review\n\n## Blockers\n\n## Suggestions\n- a suggestion\n\nVERDICT: comment\n"
	got := parseReviewNotes(md, "Blockers", "blocker")
	if len(got) != 0 {
		t.Errorf("empty section should return nil, got %v", got)
	}
}

func TestParseFollowUps(t *testing.T) {
	got := parseFollowUps(sampleReview)
	if len(got) != 2 {
		t.Errorf("got %d follow-ups, want 2 (summary paragraph must be skipped)", len(got))
	}
	if got[0].Title != "Refactor oldPkg/legacy.go" {
		t.Errorf("title[0] = %q", got[0].Title)
	}
	if len(got[0].Labels) != 2 || got[0].Labels[0] != "tech-debt" {
		t.Errorf("labels[0] = %v", got[0].Labels)
	}
	if got[1].Title != "Add integration test for the Foo flow" {
		t.Errorf("title[1] = %q", got[1].Title)
	}
	if len(got[1].Labels) != 1 || got[1].Labels[0] != "testing" {
		t.Errorf("labels[1] = %v", got[1].Labels)
	}
}

func TestExtractVerdict(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{sampleReview, "changes_requested"},
		{"VERDICT: approve\n", "approve"},
		{"VERDICT:   APPROVE  ", "approve"},
		{"VERDICT: comment", "comment"},
		{"VERDICT: maybe", "comment"}, // unknown defaults to comment
		{"no verdict at all", "comment"},
	}
	for _, c := range cases {
		got := extractVerdict(c.in)
		if got != c.want {
			t.Errorf("extractVerdict(%q) = %q, want %q", c.in[:min(30, len(c.in))], got, c.want)
		}
	}
}

func TestExtractSection(t *testing.T) {
	md := "# Review\n\n## Blockers\n- one\n- two\n\n## Suggestions\n- three\n"
	b := extractSection(md, "Blockers")
	if !strings.Contains(b, "- one") || !strings.Contains(b, "- two") {
		t.Errorf("blockers body wrong: %q", b)
	}
	if strings.Contains(b, "- three") {
		t.Errorf("blockers body leaked into suggestions: %q", b)
	}
	s := extractSection(md, "Suggestions")
	if !strings.Contains(s, "- three") {
		t.Errorf("suggestions body wrong: %q", s)
	}
	if x := extractSection(md, "Missing"); x != "" {
		t.Errorf("missing section should be empty, got %q", x)
	}
}

func TestReviewWriteFollowUpsCreatesFile(t *testing.T) {
	dir := t.TempDir()
	rev := &Review{
		FollowUps: []FollowUpIssue{
			{Title: "Do the thing", Body: "because reasons", Labels: []string{"a", "b"}},
			{Title: "Do another thing", Body: "also", Labels: nil},
		},
	}
	path, err := rev.WriteFollowUps(dir)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != "followups.md" {
		t.Errorf("basename = %q", filepath.Base(path))
	}
	data, _ := os.ReadFile(path)
	s := string(data)
	if !strings.Contains(s, "Do the thing") || !strings.Contains(s, "Do another thing") {
		t.Errorf("followups.md missing titles: %q", s)
	}
	if !strings.Contains(s, "labels: a, b") {
		t.Errorf("followups.md missing labels line: %q", s)
	}
}

func TestReviewWriteFollowUpsReturnsEmptyWhenNoFollowUps(t *testing.T) {
	dir := t.TempDir()
	rev := &Review{FollowUps: nil}
	path, err := rev.WriteFollowUps(dir)
	if err != nil {
		t.Errorf("err = %v", err)
	}
	if path != "" {
		t.Errorf("path should be empty when no follow-ups, got %q", path)
	}
}
