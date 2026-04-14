package agents

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aisu-ai/aidev/internal/github"
	"github.com/aisu-ai/aidev/internal/llm"
	"github.com/aisu-ai/aidev/internal/repo"
)

// scriptedToolProvider replays a fixed sequence of ToolAwareResponse
// turns, so tests can drive the runWithTools loop without an LLM.
// Each Complete call advances to the next response; the last
// response is reused for any calls past the end of the script so
// tests can assert on how many times the provider was invoked.
type scriptedToolProvider struct {
	responses []llm.ToolAwareResponse
	calls     int
	lastReq   llm.ToolAwareRequest
}

func (p *scriptedToolProvider) Name() string { return "scripted-tool" }

// Complete is required by the Provider interface but the tool-use
// path never calls it; we panic if it does to catch regressions
// where the Implementer accidentally falls back to the legacy path.
func (p *scriptedToolProvider) Complete(_ context.Context, _ llm.Request) (llm.Response, error) {
	panic("scriptedToolProvider.Complete called — Implementer should be using CompleteWithTools")
}

func (p *scriptedToolProvider) CompleteWithTools(_ context.Context, req llm.ToolAwareRequest) (llm.ToolAwareResponse, error) {
	p.lastReq = req
	idx := p.calls
	if idx >= len(p.responses) {
		idx = len(p.responses) - 1
	}
	p.calls++
	return p.responses[idx], nil
}

// Compile-time assertion.
var _ llm.ToolAwareProvider = (*scriptedToolProvider)(nil)

func makeTextBlock(s string) llm.ToolContentBlock {
	return llm.ToolContentBlock{Type: "text", Text: s}
}

func makeToolUseBlock(id, name, inputJSON string) llm.ToolContentBlock {
	return llm.ToolContentBlock{
		Type:      "tool_use",
		ToolUseID: id,
		ToolName:  name,
		ToolInput: json.RawMessage(inputJSON),
	}
}

// TestImplementerRunUsesToolPathWhenAvailable is the happy-path
// regression guard for v0.4. A provider that implements
// ToolAwareProvider must make Implementer.Run dispatch to the
// tool-use loop — no picker, no NEED_FILES, just tool calls and a
// final diff.
func TestImplementerRunUsesToolPathWhenAvailable(t *testing.T) {
	dir := t.TempDir()
	citedPath := "apps/marketing/src/components/pages/pricing/pricing.tsx"
	if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(citedPath)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, citedPath),
		[]byte("import { t } from '@/lib/i18n'\nexport const Pricing = () => <>{t('contactSales')}</>\n"),
		0o644); err != nil {
		t.Fatal(err)
	}

	// Scripted conversation:
	//   Turn 1: model calls read_file to see the TSX file
	//   Turn 2: model emits the final diff
	provider := &scriptedToolProvider{
		responses: []llm.ToolAwareResponse{
			{
				StopReason: "tool_use",
				ToolUses: []llm.ToolUse{
					{ID: "tu_1", Name: "read_file", Input: json.RawMessage(`{"path":"` + citedPath + `"}`)},
				},
				AssistantMessage: llm.ToolMessage{
					Role: "assistant",
					Content: []llm.ToolContentBlock{
						makeTextBlock("Let me read the pricing component."),
						makeToolUseBlock("tu_1", "read_file", `{"path":"`+citedPath+`"}`),
					},
				},
			},
			{
				StopReason: "end_turn",
				Content: "diff --git a/" + citedPath + " b/" + citedPath + "\n" +
					"--- a/" + citedPath + "\n" +
					"+++ b/" + citedPath + "\n" +
					"@@ -1,2 +1,2 @@\n" +
					" import { t } from '@/lib/i18n'\n" +
					"-export const Pricing = () => <>{t('contactSales')}</>\n" +
					"+export const Pricing = () => <>{t('common.cta.contactSales')}</>\n",
				AssistantMessage: llm.ToolMessage{
					Role: "assistant",
					Content: []llm.ToolContentBlock{
						makeTextBlock("diff --git a/" + citedPath + " b/" + citedPath + "\n..."),
					},
				},
			},
		},
	}

	impl := &Implementer{Provider: provider}
	ctx := &Context{
		Issue:    &github.Issue{Owner: "a", Repo: "b", Number: 1, Title: "t", Body: "body"},
		Snapshot: &repo.Snapshot{Root: dir},
	}
	sketch := &Sketch{Number: 1, Title: "consolidate", Markdown: "## Plan\n- update pricing"}

	patch, err := impl.Run(context.Background(), ctx, sketch)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if patch == nil {
		t.Fatal("nil patch")
	}
	if !strings.Contains(patch.Diff, "common.cta.contactSales") {
		t.Errorf("patch missing expected content, got:\n%s", patch.Diff)
	}
	if provider.calls != 2 {
		t.Errorf("provider called %d times, want 2", provider.calls)
	}
	// Sanity check: the last request's Messages should include
	// the tool_result block from executing read_file.
	foundToolResult := false
	for _, m := range provider.lastReq.Messages {
		for _, b := range m.Content {
			if b.Type == "tool_result" && b.ToolUseID == "tu_1" {
				foundToolResult = true
				if b.ToolResultIsError {
					t.Errorf("read_file reported error on an existing file: %s", b.ToolResultContent)
				}
				if !strings.Contains(b.ToolResultContent, "contactSales") {
					t.Errorf("tool_result should contain file content; got: %s", b.ToolResultContent)
				}
			}
		}
	}
	if !foundToolResult {
		t.Error("last request did not contain a tool_result block for tu_1")
	}
}

// TestImplementerRunToolPathMultipleIterations exercises a realistic
// multi-turn conversation: glob → read_file (two calls in one turn)
// → grep → emit diff. Four provider calls total, with multiple
// tool_use blocks in some assistant turns.
func TestImplementerRunToolPathMultipleIterations(t *testing.T) {
	dir := t.TempDir()
	must := func(path, body string) {
		full := filepath.Join(dir, path)
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
		_ = os.WriteFile(full, []byte(body), 0o644)
	}
	must("apps/marketing/src/components/a.tsx", "export const A = () => t('contactSales')\n")
	must("apps/marketing/src/components/b.tsx", "export const B = () => t('contactSales')\n")
	must("apps/marketing/src/lib/i18n/translations/en/common.json", `{"contactSales":"Contact Sales"}`+"\n")

	provider := &scriptedToolProvider{
		responses: []llm.ToolAwareResponse{
			// Turn 1: one glob to find the components
			{
				StopReason: "tool_use",
				ToolUses: []llm.ToolUse{
					{ID: "tu_glob", Name: "glob", Input: json.RawMessage(`{"pattern":"apps/marketing/**/*.tsx"}`)},
				},
				AssistantMessage: llm.ToolMessage{
					Role: "assistant",
					Content: []llm.ToolContentBlock{
						makeToolUseBlock("tu_glob", "glob", `{"pattern":"apps/marketing/**/*.tsx"}`),
					},
				},
			},
			// Turn 2: two read_file calls in the same turn (batched)
			{
				StopReason: "tool_use",
				ToolUses: []llm.ToolUse{
					{ID: "tu_a", Name: "read_file", Input: json.RawMessage(`{"path":"apps/marketing/src/components/a.tsx"}`)},
					{ID: "tu_b", Name: "read_file", Input: json.RawMessage(`{"path":"apps/marketing/src/components/b.tsx"}`)},
				},
				AssistantMessage: llm.ToolMessage{
					Role: "assistant",
					Content: []llm.ToolContentBlock{
						makeToolUseBlock("tu_a", "read_file", `{"path":"apps/marketing/src/components/a.tsx"}`),
						makeToolUseBlock("tu_b", "read_file", `{"path":"apps/marketing/src/components/b.tsx"}`),
					},
				},
			},
			// Turn 3: grep to confirm no other consumers
			{
				StopReason: "tool_use",
				ToolUses: []llm.ToolUse{
					{ID: "tu_grep", Name: "grep", Input: json.RawMessage(`{"pattern":"contactSales","paths":["apps/marketing/src"]}`)},
				},
				AssistantMessage: llm.ToolMessage{
					Role: "assistant",
					Content: []llm.ToolContentBlock{
						makeToolUseBlock("tu_grep", "grep", `{"pattern":"contactSales","paths":["apps/marketing/src"]}`),
					},
				},
			},
			// Turn 4: emit the final diff
			{
				StopReason: "end_turn",
				Content: "diff --git a/apps/marketing/src/components/a.tsx b/apps/marketing/src/components/a.tsx\n" +
					"--- a/apps/marketing/src/components/a.tsx\n" +
					"+++ b/apps/marketing/src/components/a.tsx\n" +
					"@@ -1 +1 @@\n" +
					"-export const A = () => t('contactSales')\n" +
					"+export const A = () => t('common.cta.contactSales')\n",
				AssistantMessage: llm.ToolMessage{
					Role:    "assistant",
					Content: []llm.ToolContentBlock{makeTextBlock("diff --git a/apps/marketing/src/components/a.tsx b/apps/marketing/src/components/a.tsx\n...")},
				},
			},
		},
	}

	impl := &Implementer{Provider: provider}
	ctx := &Context{
		Issue:    &github.Issue{Owner: "a", Repo: "b", Number: 1, Title: "t", Body: "body"},
		Snapshot: &repo.Snapshot{Root: dir},
	}
	sketch := &Sketch{Number: 1, Title: "consolidate", Markdown: "## Plan"}

	patch, err := impl.Run(context.Background(), ctx, sketch)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if patch == nil {
		t.Fatal("nil patch")
	}
	if provider.calls != 4 {
		t.Errorf("provider called %d times, want 4 (glob, 2x read_file, grep, end)", provider.calls)
	}
}

// TestImplementerRunToolPathHitsIterationCap guards against runaway
// loops. A provider that keeps emitting tool_use forever must trip
// maxToolIterations and return an error instead of ballooning cost.
func TestImplementerRunToolPathHitsIterationCap(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "x.go"), []byte("package x\n"), 0o644)

	// Single response that keeps requesting read_file on a real
	// file — the loop has no termination path.
	looping := llm.ToolAwareResponse{
		StopReason: "tool_use",
		ToolUses: []llm.ToolUse{
			{ID: "tu_loop", Name: "read_file", Input: json.RawMessage(`{"path":"x.go"}`)},
		},
		AssistantMessage: llm.ToolMessage{
			Role: "assistant",
			Content: []llm.ToolContentBlock{
				makeToolUseBlock("tu_loop", "read_file", `{"path":"x.go"}`),
			},
		},
	}
	script := make([]llm.ToolAwareResponse, maxToolIterations+5)
	for i := range script {
		script[i] = looping
	}
	provider := &scriptedToolProvider{responses: script}

	impl := &Implementer{Provider: provider}
	ctx := &Context{
		Issue:    &github.Issue{Owner: "a", Repo: "b", Number: 1, Title: "t", Body: "body"},
		Snapshot: &repo.Snapshot{Root: dir},
	}
	sketch := &Sketch{Number: 1, Title: "t", Markdown: "m"}

	_, err := impl.Run(context.Background(), ctx, sketch)
	if err == nil {
		t.Fatal("expected iteration-cap error, got nil")
	}
	if !strings.Contains(err.Error(), "exhausted") {
		t.Errorf("expected 'exhausted' in error, got: %v", err)
	}
	if provider.calls != maxToolIterations {
		t.Errorf("provider called %d times, want exactly %d (the cap)", provider.calls, maxToolIterations)
	}
}

// TestImplementerRunToolPathSurfacesERROR verifies the ERROR escape
// hatch still works when the model emits ERROR: as its final text.
// Even though the tool-use path eliminates most reasons to ERROR,
// genuine impossibility is still a valid terminal state.
func TestImplementerRunToolPathSurfacesERROR(t *testing.T) {
	dir := t.TempDir()

	provider := &scriptedToolProvider{
		responses: []llm.ToolAwareResponse{
			{
				StopReason: "end_turn",
				Content:    "ERROR: the sketch contradicts itself — cannot reconcile rules A and B",
				AssistantMessage: llm.ToolMessage{
					Role: "assistant",
					Content: []llm.ToolContentBlock{
						makeTextBlock("ERROR: the sketch contradicts itself — cannot reconcile rules A and B"),
					},
				},
			},
		},
	}
	impl := &Implementer{Provider: provider}
	ctx := &Context{
		Issue:    &github.Issue{Owner: "a", Repo: "b", Number: 1, Title: "t", Body: "body"},
		Snapshot: &repo.Snapshot{Root: dir},
	}
	sketch := &Sketch{Number: 1, Title: "t", Markdown: "m"}

	_, err := impl.Run(context.Background(), ctx, sketch)
	if err == nil {
		t.Fatal("expected declared ERROR, got nil")
	}
	if !strings.Contains(err.Error(), "contradicts itself") {
		t.Errorf("ERROR reason should be surfaced; got: %v", err)
	}
}

// TestImplementerRunToolPathTolerantToProsePreamble verifies the
// validator accepts a diff that's prefixed with a short narrative.
// The tool-use path is more forgiving than the legacy path because
// it has no NEED_FILES grammar to disambiguate against.
func TestImplementerRunToolPathTolerantToProsePreamble(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "x.go"), []byte("package x\n"), 0o644)

	provider := &scriptedToolProvider{
		responses: []llm.ToolAwareResponse{
			{
				StopReason: "end_turn",
				Content: "Here's the diff that consolidates the call sites:\n\n" +
					"diff --git a/x.go b/x.go\n" +
					"--- a/x.go\n" +
					"+++ b/x.go\n" +
					"@@ -1 +1 @@\n" +
					"-package x\n" +
					"+package y\n",
				AssistantMessage: llm.ToolMessage{
					Role:    "assistant",
					Content: []llm.ToolContentBlock{makeTextBlock("Here's the diff...")},
				},
			},
		},
	}
	impl := &Implementer{Provider: provider}
	ctx := &Context{
		Issue:    &github.Issue{Owner: "a", Repo: "b", Number: 1, Title: "t", Body: "body"},
		Snapshot: &repo.Snapshot{Root: dir},
	}
	sketch := &Sketch{Number: 1, Title: "t", Markdown: "m"}

	patch, err := impl.Run(context.Background(), ctx, sketch)
	if err != nil {
		t.Fatalf("prose preamble should be tolerated: %v", err)
	}
	if !strings.Contains(patch.Diff, "+package y") {
		t.Errorf("patch should start at 'diff --git', got:\n%s", patch.Diff)
	}
	// The preamble must be stripped — the Patch.Diff field starts
	// at diff --git, not at "Here's the diff".
	if strings.Contains(patch.Diff, "Here's the diff") {
		t.Errorf("preamble should have been stripped; got:\n%s", patch.Diff)
	}
}

// TestImplementerRunLegacyPathStillFiresForNonToolProviders is the
// critical backwards-compat guard: a provider that does NOT
// implement ToolAwareProvider must go through the legacy picker +
// NEED_FILES + harvest path. The scripted Provider (not the
// scriptedToolProvider) serves as a non-tool-aware fixture.
func TestImplementerRunLegacyPathStillFiresForNonToolProviders(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "x.go"), []byte("package x\n"), 0o644)

	// scriptedProvider is from implementer_test.go and does NOT
	// implement ToolAwareProvider. A type assertion in Run() must
	// fall through to runLegacy and produce a patch via the picker
	// + generateDiff path.
	provider := &scriptedProvider{
		responses: []string{
			`["x.go"]`, // picker
			"diff --git a/x.go b/x.go\n--- a/x.go\n+++ b/x.go\n@@ -1 +1 @@\n-package x\n+package y\n",
		},
	}
	impl := &Implementer{Provider: provider}
	ctx := &Context{
		Issue:    &github.Issue{Owner: "a", Repo: "b", Number: 1, Title: "t", Body: "body"},
		Snapshot: &repo.Snapshot{Root: dir},
	}
	sketch := &Sketch{Number: 1, Title: "t", Markdown: "m"}

	patch, err := impl.Run(context.Background(), ctx, sketch)
	if err != nil {
		t.Fatalf("legacy path should still work: %v", err)
	}
	if patch == nil {
		t.Fatal("nil patch from legacy path")
	}
	if !strings.Contains(patch.Diff, "+package y") {
		t.Errorf("legacy diff wrong: %s", patch.Diff)
	}
}
