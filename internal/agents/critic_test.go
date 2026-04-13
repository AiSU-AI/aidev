package agents

import (
	"context"
	"strings"
	"testing"

	"github.com/aisu-ai/aidev/internal/github"
	"github.com/aisu-ai/aidev/internal/llm"
)

// capturingProvider records the last Request it was asked to
// complete and returns a canned Response. Tests use it to assert
// prompt-assembly behaviour without a real model.
type capturingProvider struct {
	req   llm.Request
	reply string
}

func (p *capturingProvider) Name() string { return "capturing" }
func (p *capturingProvider) Complete(_ context.Context, req llm.Request) (llm.Response, error) {
	p.req = req
	return llm.Response{Content: p.reply}, nil
}

func TestExtractRecommendation(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"explicit build", "long report\n\nRECOMMENDATION: build\n", "build"},
		{"defer mixed case", "body\nRecommendation: Defer\n", "defer"},
		{"kill with trailing spaces", "body\nRECOMMENDATION:   kill  ", "kill"},
		{"unclear preserved", "body\nRECOMMENDATION: unclear", "unclear"},
		{"missing defaults unclear", "just some body without a verdict", "unclear"},
		{"unknown verb defaults unclear", "body\nRECOMMENDATION: maybe", "unclear"},
	}
	for _, c := range cases {
		got := extractRecommendation(c.in)
		if got != c.want {
			t.Errorf("%s: extractRecommendation = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestCriticPromptIncludesClarifierNotes verifies that when
// Context.ClarifierNotes is non-empty, the Critic embeds it into the
// user-message under the "Clarifier session" heading so the model
// sees the human's authoritative answers on its second pass. This is
// the regression guard for the headless interview loop — without it,
// clarifying wouldn't actually change the Critic's verdict on
// re-critique.
func TestCriticPromptIncludesClarifierNotes(t *testing.T) {
	p := &capturingProvider{reply: "body\nRECOMMENDATION: build\n"}
	c := &Critic{Provider: p}
	cc := &Context{
		Issue: &github.Issue{
			Owner: "aisu-ai", Repo: "aidev", Number: 1,
			Title: "T", Body: "issue body",
		},
		ScoutReport:    "scout brief body",
		ClarifierNotes: "- **Q (q1):** is this a pipeline exercise?\n  **A:** yes, fix the drift only\n",
	}
	if _, err := c.Run(context.Background(), cc); err != nil {
		t.Fatalf("Critic.Run error: %v", err)
	}
	userMsg := p.req.Messages[0].Content
	if !strings.Contains(userMsg, "## Clarifier session") {
		t.Errorf("critic user message missing '## Clarifier session' heading; got:\n%s", userMsg)
	}
	if !strings.Contains(userMsg, "is this a pipeline exercise?") {
		t.Errorf("critic user message missing the clarifier Q text; got:\n%s", userMsg)
	}
	if !strings.Contains(userMsg, "yes, fix the drift only") {
		t.Errorf("critic user message missing the clarifier A text; got:\n%s", userMsg)
	}
}

// TestCriticPromptOmitsClarifierSectionWhenEmpty is the negative
// case: if ClarifierNotes is empty, the user message must NOT contain
// the Clarifier heading (otherwise we'd be teaching the model to
// expect authoritative human input that isn't there).
func TestCriticPromptOmitsClarifierSectionWhenEmpty(t *testing.T) {
	p := &capturingProvider{reply: "body\nRECOMMENDATION: unclear\n"}
	c := &Critic{Provider: p}
	cc := &Context{
		Issue: &github.Issue{
			Owner: "aisu-ai", Repo: "aidev", Number: 1,
			Title: "T", Body: "issue body",
		},
		ScoutReport: "scout brief body",
	}
	if _, err := c.Run(context.Background(), cc); err != nil {
		t.Fatalf("Critic.Run error: %v", err)
	}
	if strings.Contains(p.req.Messages[0].Content, "## Clarifier session") {
		t.Error("critic user message should not contain '## Clarifier session' when ClarifierNotes is empty")
	}
}
