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

// recordingToolProvider captures every ToolAwareRequest it
// receives so tests can inspect the full multi-turn
// conversation. Used by the format-correction tests below to
// verify that the corrective follow-up actually quoted the
// broken output back to the model.
type recordingToolProvider struct {
	responses []llm.ToolAwareResponse
	requests  []llm.ToolAwareRequest
}

func (p *recordingToolProvider) Name() string { return "recording-tool" }
func (p *recordingToolProvider) Complete(_ context.Context, _ llm.Request) (llm.Response, error) {
	panic("recordingToolProvider.Complete called")
}
func (p *recordingToolProvider) CompleteWithTools(_ context.Context, req llm.ToolAwareRequest) (llm.ToolAwareResponse, error) {
	p.requests = append(p.requests, req)
	idx := len(p.requests) - 1
	if idx >= len(p.responses) {
		idx = len(p.responses) - 1
	}
	return p.responses[idx], nil
}

var _ llm.ToolAwareProvider = (*recordingToolProvider)(nil)

// TestImplementerRunToolPathRecoversFromMalformedFinalContent
// is the regression guard for v0.5a. When the model ends its
// turn with content that isn't a diff (and not an ERROR:), the
// harness must send a corrective follow-up and let the model
// produce a valid diff on the retry.
//
// Covers the exact scenario the user hit: qwen2.5-coder:14b
// emitted a `{` on its final turn instead of a `diff --git`.
// The embedded-tool-call salvage in fromOllamaResponse catches
// the most common subset of this failure mode, but when the
// content doesn't match any known tool-call shape (e.g. the
// model wrote `{"diff": "..."}` or just random JSON garbage)
// the format-correction retry is the backstop.
func TestImplementerRunToolPathRecoversFromMalformedFinalContent(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	provider := &recordingToolProvider{
		responses: []llm.ToolAwareResponse{
			// Turn 1: model ends with malformed content (a JSON
			// wrapper around the diff — not a tool-call shape,
			// not a valid diff).
			{
				StopReason: "end_turn",
				Content:    `{"diff": "diff --git a/x.go b/x.go\n--- a/x.go\n+++ b/x.go\n@@ -1 +1 @@\n-package x\n+package y\n"}`,
				AssistantMessage: llm.ToolMessage{
					Role: "assistant",
					Content: []llm.ToolContentBlock{
						makeTextBlock(`{"diff": "..."}`),
					},
				},
			},
			// Turn 2: after the corrective follow-up, the model
			// emits a real unified diff.
			{
				StopReason: "end_turn",
				Content: "diff --git a/x.go b/x.go\n" +
					"--- a/x.go\n" +
					"+++ b/x.go\n" +
					"@@ -1 +1 @@\n" +
					"-package x\n" +
					"+package y\n",
				AssistantMessage: llm.ToolMessage{
					Role:    "assistant",
					Content: []llm.ToolContentBlock{makeTextBlock("diff --git a/x.go b/x.go\n...")},
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
		t.Fatalf("Run error: %v", err)
	}
	if patch == nil {
		t.Fatal("nil patch")
	}
	if !strings.Contains(patch.Diff, "+package y") {
		t.Errorf("final patch wrong: %s", patch.Diff)
	}

	// Two turns total: one rejected + one corrected.
	if len(provider.requests) != 2 {
		t.Fatalf("provider called %d times, want 2", len(provider.requests))
	}

	// The SECOND turn's user message list must end with a
	// corrective user message that quotes the broken content.
	secondReq := provider.requests[1]
	if len(secondReq.Messages) == 0 {
		t.Fatal("second request has no messages")
	}
	lastMsg := secondReq.Messages[len(secondReq.Messages)-1]
	if lastMsg.Role != "user" {
		t.Errorf("last message role = %q, want user", lastMsg.Role)
	}
	var foundCorrection bool
	for _, b := range lastMsg.Content {
		if b.Type == "text" && strings.Contains(b.Text, "Your previous response was not a valid final output") {
			foundCorrection = true
		}
		if b.Type == "text" && !strings.Contains(b.Text, `{"diff":`) {
			continue
		}
		if b.Type == "text" && strings.Contains(b.Text, `{"diff":`) {
			// The correction message should echo the broken
			// JSON wrapper back to the model so it sees what
			// it wrote.
			foundCorrection = true
		}
	}
	if !foundCorrection {
		t.Errorf("corrective follow-up missing or malformed; last message:\n%+v", lastMsg)
	}
}

// TestImplementerRunToolPathFailsAfterRepeatedMalformedOutput
// guards the format-correction cap. A model that refuses to
// emit a valid diff across multiple corrections must fail
// hard with a clear error that includes the raw output for
// debugging.
func TestImplementerRunToolPathFailsAfterRepeatedMalformedOutput(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Every response is malformed — no diff marker anywhere.
	malformed := llm.ToolAwareResponse{
		StopReason: "end_turn",
		Content:    `{"i": "wrote", "more": "nonsense"}`,
		AssistantMessage: llm.ToolMessage{
			Role:    "assistant",
			Content: []llm.ToolContentBlock{makeTextBlock(`{"i": "wrote", "more": "nonsense"}`)},
		},
	}
	script := make([]llm.ToolAwareResponse, maxToolFormatCorrections+3)
	for i := range script {
		script[i] = malformed
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
		t.Fatal("expected hard failure after format-correction cap")
	}
	// The error must include the raw output so the user can
	// debug without re-running — this is the debuggability
	// fix #35/#36 were missing at the tool-use path.
	if !strings.Contains(err.Error(), "raw output:") {
		t.Errorf("error should include raw output section; got: %v", err)
	}
	if !strings.Contains(err.Error(), `{"i": "wrote"`) {
		t.Errorf("error should quote the malformed content; got: %v", err)
	}
	if !strings.Contains(err.Error(), "format corrections") {
		t.Errorf("error should mention format corrections; got: %v", err)
	}
}

// TestImplementerRunToolPathFormatCorrectionNotTriggeredOnERROR
// verifies that an ERROR: escape hatch from the model is
// surfaced immediately without going through format
// correction. ERROR: is an intentional terminal state, not a
// malformed output.
func TestImplementerRunToolPathFormatCorrectionNotTriggeredOnERROR(t *testing.T) {
	dir := t.TempDir()

	provider := &scriptedToolProvider{
		responses: []llm.ToolAwareResponse{
			{
				StopReason: "end_turn",
				Content:    "ERROR: sketch references a file that does not exist",
				AssistantMessage: llm.ToolMessage{
					Role:    "assistant",
					Content: []llm.ToolContentBlock{makeTextBlock("ERROR: sketch references a file that does not exist")},
				},
			},
			// This second response should NEVER be reached —
			// ERROR: terminates immediately.
			{
				StopReason: "end_turn",
				Content:    "diff --git a/x b/x\n",
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
		t.Fatal("ERROR: should propagate as a Go error")
	}
	if !strings.Contains(err.Error(), "declared ERROR") {
		t.Errorf("ERROR: should surface as 'declared ERROR', got: %v", err)
	}
	// Only one provider call — the second scripted response
	// must not have been reached.
	if provider.calls != 1 {
		t.Errorf("provider called %d times, want 1 (ERROR should terminate immediately)", provider.calls)
	}
}
