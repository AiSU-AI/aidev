package agents

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aisu-ai/aidev/internal/github"
	"github.com/aisu-ai/aidev/internal/llm"
	"github.com/aisu-ai/aidev/internal/repo"
)

// scriptedProvider returns a canned response for each Complete call
// in order. The last response is used for every call past the end of
// the script so tests can decouple "how many LLM calls happened" from
// "what the scripted turns look like" — the loop termination assertion
// is what we're checking.
type scriptedProvider struct {
	responses []string
	calls     int
}

func (p *scriptedProvider) Name() string { return "scripted" }
func (p *scriptedProvider) Complete(_ context.Context, _ llm.Request) (llm.Response, error) {
	idx := p.calls
	if idx >= len(p.responses) {
		idx = len(p.responses) - 1
	}
	p.calls++
	return llm.Response{Content: p.responses[idx]}, nil
}

func TestParseFilesFromDiff(t *testing.T) {
	diff := `diff --git a/cmd/main.go b/cmd/main.go
--- a/cmd/main.go
+++ b/cmd/main.go
@@ -1,3 +1,4 @@
 package main
+import "fmt"

diff --git a/internal/foo.go b/internal/foo.go
--- /dev/null
+++ b/internal/foo.go
@@ -0,0 +1,3 @@
+package internal
+

diff --git a/deleted.go b/deleted.go
--- a/deleted.go
+++ /dev/null
`
	got := parseFilesFromDiff(diff)
	want := []string{"cmd/main.go", "internal/foo.go"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("got[%d] = %q, want %q", i, got[i], w)
		}
	}
}

func TestParseFilesFromDiffSkipsDevNullTargets(t *testing.T) {
	diff := `diff --git a/x.go b/x.go
--- a/x.go
+++ /dev/null
`
	got := parseFilesFromDiff(diff)
	if len(got) != 0 {
		t.Errorf("should not have recorded deleted file, got %v", got)
	}
}

func TestStripCodeFenceRemovesMarkdownWrap(t *testing.T) {
	wrapped := "```diff\ndiff --git a/x b/x\n@@ -1 +1 @@\n-a\n+b\n```"
	got := stripCodeFence(wrapped)
	if strings.HasPrefix(got, "```") || strings.HasSuffix(got, "```") {
		t.Errorf("fences not removed: %q", got)
	}
	if !strings.HasPrefix(got, "diff --git") {
		t.Errorf("diff content corrupted: %q", got)
	}
}

func TestStripCodeFenceLeavesUnwrappedAlone(t *testing.T) {
	raw := "diff --git a/x b/x\n@@ -1 +1 @@\n-a\n+b"
	if got := stripCodeFence(raw); got != raw {
		t.Errorf("unwrapped content should be unchanged, got %q", got)
	}
}

func TestPatchWriteToCreatesAidevDirAndFile(t *testing.T) {
	dir := t.TempDir()
	p := &Patch{Diff: "diff --git a/x b/x\n@@ -1 +1 @@\n-a\n+b"}
	path, err := p.WriteTo(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, ".aidev", "proposed.patch")
	if path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	if p.Path != path {
		t.Errorf("p.Path not updated: %q", p.Path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "diff --git") {
		t.Error("patch file missing diff")
	}
	if !strings.Contains(string(data), "Written by aidev") {
		t.Error("patch file missing footer stamp")
	}
}

func TestListSourceFilesRespectsLimit(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 20; i++ {
		name := filepath.Join(dir, "file"+intoa(i)+".go")
		if err := os.WriteFile(name, []byte("package x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := listSourceFiles(dir, 5)
	if len(got) != 5 {
		t.Errorf("got %d files, want 5 (limit)", len(got))
	}
}

func TestListSourceFilesSkipsVendorDirs(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "node_modules", "foo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "node_modules", "foo", "x.js"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := listSourceFiles(dir, 100)
	for _, f := range got {
		if strings.Contains(f, "node_modules") {
			t.Errorf("should have skipped node_modules: %q", f)
		}
	}
	if len(got) != 1 || got[0] != "main.go" {
		t.Errorf("got %v, want [main.go]", got)
	}
}

func TestListSourceFilesSkipsNonSourceExtensions(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "image.png"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := listSourceFiles(dir, 100)
	if len(got) != 1 || got[0] != "main.go" {
		t.Errorf("got %v, want [main.go]", got)
	}
}

func intoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// v0.3a.1 two-turn Implementer tests.

func TestParseFileSelectionBareJSON(t *testing.T) {
	got, err := parseFileSelection(`["cmd/main.go", "internal/foo.go"]`)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "cmd/main.go" || got[1] != "internal/foo.go" {
		t.Errorf("got %v", got)
	}
}

func TestParseFileSelectionWrappedInCodeFence(t *testing.T) {
	raw := "```json\n[\"a.go\", \"b.go\"]\n```"
	got, err := parseFileSelection(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("got %v", got)
	}
}

func TestParseFileSelectionWithSurroundingProse(t *testing.T) {
	raw := "Here are the files I need:\n\n[\"main.go\"]\n\nLet me know if you need more."
	got, err := parseFileSelection(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "main.go" {
		t.Errorf("got %v", got)
	}
}

func TestParseFileSelectionEmptyErrors(t *testing.T) {
	if _, err := parseFileSelection(""); err == nil {
		t.Error("expected error on empty input")
	}
}

func TestParseFileSelectionNoJSONErrors(t *testing.T) {
	if _, err := parseFileSelection("sorry, I can't help with that"); err == nil {
		t.Error("expected error when no JSON array is present")
	}
}

func TestCleanFileListDropsUnsafePaths(t *testing.T) {
	in := []string{
		"good/path.go",
		"/absolute/path.go",       // absolute — drop
		"../escape.go",            // escape — drop
		"nested/../evil.go",       // escape — drop
		"good/path.go",            // duplicate — drop
		"another.go",
	}
	got := cleanFileList(in)
	want := []string{"good/path.go", "another.go"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("got[%d] = %q, want %q", i, got[i], w)
		}
	}
}

func TestCleanFileListCapsLength(t *testing.T) {
	// Generate comfortably more than maxRequestedFiles so the cap is
	// actually exercised rather than the slice just being the same
	// size as the limit.
	in := make([]string, maxRequestedFiles*2)
	for j := range in {
		in[j] = "file" + intoa(j) + ".go"
	}
	got := cleanFileList(in)
	if len(got) != maxRequestedFiles {
		t.Errorf("got %d, want %d", len(got), maxRequestedFiles)
	}
}

func TestIsSafeRelPath(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"foo/bar.go", true},
		{"foo.go", true},
		{"", false},
		{"/abs/path", false},
		{"../escape", false},
		{"foo/../escape", false},
		{"foo/./ok.go", true},
		{strings.Repeat("a", 2000), false},
	}
	for _, c := range cases {
		got := isSafeRelPath(c.in)
		if got != c.want {
			t.Errorf("isSafeRelPath(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestReadFilesSkipsMissingAndDirs(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "exists.go"), []byte("package main"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, missing := readFiles(dir, []string{"exists.go", "missing.go", "sub"})
	if len(got) != 1 {
		t.Errorf("got %d files, want 1", len(got))
	}
	if got["exists.go"] != "package main" {
		t.Errorf("content mismatch: %q", got["exists.go"])
	}
	// missing.go and sub (directory) should both be in the missing list
	// so the caller can tell the model they're unavailable.
	if len(missing) != 2 {
		t.Errorf("missing list = %v, want 2 entries", missing)
	}
	var haveMissing, haveSub bool
	for _, m := range missing {
		if m == "missing.go" {
			haveMissing = true
		}
		if m == "sub" {
			haveSub = true
		}
	}
	if !haveMissing || !haveSub {
		t.Errorf("expected missing list to contain 'missing.go' and 'sub', got %v", missing)
	}
}

func TestReadFilesTruncatesLargeFiles(t *testing.T) {
	dir := t.TempDir()
	big := strings.Repeat("x", maxFileBytes*2)
	if err := os.WriteFile(filepath.Join(dir, "big.txt"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	got, _ := readFiles(dir, []string{"big.txt"})
	content := got["big.txt"]
	if !strings.Contains(content, "truncated") {
		t.Errorf("expected truncation marker, got first 100 chars: %q", content[:100])
	}
	// Length should be bounded by the limit + marker.
	if len(content) > maxFileBytes+500 {
		t.Errorf("truncated content too long: %d bytes", len(content))
	}
}

func TestReadFilesRefusesUnsafePaths(t *testing.T) {
	dir := t.TempDir()
	// Create a file OUTSIDE the intended root that an escape path
	// could try to reach.
	parent := filepath.Dir(dir)
	secret := filepath.Join(parent, "secret.txt")
	if err := os.WriteFile(secret, []byte("top secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(secret)

	got, missing := readFiles(dir, []string{"../" + filepath.Base(secret), "/etc/passwd"})
	if len(got) != 0 {
		t.Errorf("unsafe paths should have been rejected, got %v", got)
	}
	// Unsafe paths are silently dropped at the boundary — they do
	// NOT go into the "missing" list, because we don't want to
	// surface red-flag paths back to the model. The missing list is
	// for legitimate typos and hallucinations, not for probing.
	if len(missing) != 0 {
		t.Errorf("unsafe paths should not be reported as 'missing', got %v", missing)
	}
}

func TestSortStrings(t *testing.T) {
	a := []string{"c", "a", "b"}
	sortStrings(a)
	if a[0] != "a" || a[1] != "b" || a[2] != "c" {
		t.Errorf("sortStrings result: %v", a)
	}
}

// parseNeedFiles tests — the NEED_FILES protocol is the
// Implementer's recourse when the first-turn picker under-specified
// and the model needs more repo context to produce a valid diff.
// Tolerance for formatting variations (prose, stray fences, extra
// whitespace) matters because models don't emit these directives
// cleanly on every response.

func TestParseNeedFilesBareDirective(t *testing.T) {
	got, err := parseNeedFiles(`NEED_FILES: ["de/common.json", "fr/common.json"]`)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "de/common.json" || got[1] != "fr/common.json" {
		t.Errorf("got %v", got)
	}
}

func TestParseNeedFilesWithExtraWhitespace(t *testing.T) {
	got, err := parseNeedFiles("   NEED_FILES:    [\"a.tsx\" , \"b.tsx\"]\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "a.tsx" || got[1] != "b.tsx" {
		t.Errorf("got %v", got)
	}
}

func TestParseNeedFilesRejectsMissingArray(t *testing.T) {
	_, err := parseNeedFiles("NEED_FILES:")
	if err == nil {
		t.Error("expected error when the directive has no JSON array")
	}
}

func TestParseNeedFilesRejectsBrokenJSON(t *testing.T) {
	_, err := parseNeedFiles(`NEED_FILES: [not valid json]`)
	if err == nil {
		t.Error("expected error on broken JSON")
	}
}

func TestParseNeedFilesDropsUnsafePaths(t *testing.T) {
	got, err := parseNeedFiles(`NEED_FILES: ["../../etc/passwd", "ok.go", "/abs/path"]`)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "ok.go" {
		t.Errorf("expected only ok.go to survive sanitisation, got %v", got)
	}
}

// TestImplementerRunSucceedsAfterNeedFiles drives the agentic loop:
// the picker names one file, the first generateDiff turn requests
// MORE files via NEED_FILES, and the second generateDiff turn emits
// the real diff. Success means the Run method followed the
// three-turn dance and came back with a Patch.
func TestImplementerRunSucceedsAfterNeedFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "locales", "en"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "locales", "fr"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "locales", "en", "common.json"), []byte(`{"contactSales":"Contact Sales"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "locales", "fr", "common.json"), []byte(`{"contactSales":"Contacter les ventes"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	provider := &scriptedProvider{
		responses: []string{
			// Turn 1 (picker): request the English locale file only.
			`["locales/en/common.json"]`,
			// Turn 2 (generateDiff, round 0): model realises it also
			// needs the French file to copy the canonical translation.
			`NEED_FILES: ["locales/fr/common.json"]`,
			// Turn 2 (round 1): real diff now that both files are
			// loaded.
			"diff --git a/locales/en/common.json b/locales/en/common.json\n" +
				"--- a/locales/en/common.json\n" +
				"+++ b/locales/en/common.json\n" +
				"@@ -1 +1 @@\n" +
				"-{\"contactSales\":\"Contact Sales\"}\n" +
				"+{\"cta\":{\"contactSales\":\"Contact Sales\"}}\n",
		},
	}
	impl := &Implementer{Provider: provider}
	ctx := &Context{
		Issue: &github.Issue{
			Owner: "aisu-ai", Repo: "aidev", Number: 1,
			Title: "T", Body: "consolidate sales CTA across locales",
		},
		Snapshot: &repo.Snapshot{Root: dir},
	}
	sketch := &Sketch{Number: 1, Title: "manual consolidation", Markdown: "## Plan\n- add common.cta.contactSales"}

	patch, err := impl.Run(context.Background(), ctx, sketch)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if patch == nil {
		t.Fatal("nil patch on success")
	}
	if !strings.Contains(patch.Diff, "Contact Sales") {
		t.Errorf("patch missing canonical value, got:\n%s", patch.Diff)
	}
	// Sanity-check the loop ran the expected number of turns: picker
	// + first generateDiff (NEED_FILES) + second generateDiff (diff).
	if provider.calls != 3 {
		t.Errorf("provider called %d times, want 3", provider.calls)
	}
}

// TestImplementerRunRejectsInfiniteNeedFiles verifies the loop cap.
// A model that keeps saying "need more files" forever must eventually
// trip the maxNeedFilesRounds guard and fail with a clear error
// rather than ballooning LLM cost.
func TestImplementerRunRejectsInfiniteNeedFiles(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a.go", "b.go", "c.go", "d.go"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("package x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	provider := &scriptedProvider{
		responses: []string{
			`["a.go"]`,                   // picker
			`NEED_FILES: ["b.go"]`,       // round 0
			`NEED_FILES: ["c.go"]`,       // round 1
			`NEED_FILES: ["d.go"]`,       // round 2 — should trip the cap
			`NEED_FILES: ["unused.go"]`,  // never reached
		},
	}
	impl := &Implementer{Provider: provider}
	ctx := &Context{
		Issue:    &github.Issue{Owner: "aisu-ai", Repo: "aidev", Number: 1, Title: "T", Body: "body"},
		Snapshot: &repo.Snapshot{Root: dir},
	}
	sketch := &Sketch{Number: 1, Title: "t", Markdown: "m"}

	_, err := impl.Run(context.Background(), ctx, sketch)
	if err == nil {
		t.Fatal("expected NEED_FILES-cap error, got nil")
	}
	if !strings.Contains(err.Error(), "NEED_FILES limit reached") {
		t.Errorf("expected NEED_FILES limit error, got: %v", err)
	}
}

// TestImplementerRunRecoversFromProsePrefixViolation drives the
// format-correction retry: the Implementer's first diff-generation
// turn responds with prose ("Looking at this, I need the TSX call
// sites to anchor edits precisely...") instead of a valid prefix.
// The harness must catch the violation, send the model back with a
// corrective message that quotes the broken response, and recover
// on the retry. This is the exact failure mode the user hit on
// their real run — model had the right intent, wrong format.
func TestImplementerRunRecoversFromProsePrefixViolation(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x.tsx"), []byte("const Foo = () => <>hi</>\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	provider := &scriptedProvider{
		responses: []string{
			// Turn 1 (picker): normal JSON array.
			`["x.tsx"]`,
			// Turn 2 (generateDiff, initial): prose instead of a
			// valid prefix. This is the real-world failure mode.
			"Looking at this, I need the TSX call sites to anchor edits precisely. Could you send me the component file?",
			// Turn 2b (generateDiff, format-corrected retry):
			// the model now emits a valid NEED_FILES directive.
			`NEED_FILES: ["x.tsx"]`,
			// Turn 2c (generateDiff, post-NEED_FILES): real diff.
			// x.tsx is already in the initial picker's contents so
			// the NEED_FILES short-circuits on duplicate...
		},
	}
	impl := &Implementer{Provider: provider}
	ctx := &Context{
		Issue:    &github.Issue{Owner: "a", Repo: "b", Number: 1, Title: "T", Body: "b"},
		Snapshot: &repo.Snapshot{Root: dir},
	}
	sketch := &Sketch{Number: 1, Title: "t", Markdown: "m"}

	// The scripted NEED_FILES re-requests x.tsx which is already
	// loaded. The Run loop will reject that with
	// "already provided" — exercising BOTH the format-correction
	// path AND the dedup guard. We assert we make it that far (i.e.
	// the prose didn't kill the run) by checking the error message.
	_, err := impl.Run(context.Background(), ctx, sketch)
	if err == nil {
		t.Fatal("expected a later error, got nil")
	}
	if !strings.Contains(err.Error(), "already provided") {
		t.Errorf("expected the NEED_FILES dedup guard to fire after format recovery, got: %v", err)
	}
	// Provider should have been called for: picker(1) + first
	// generateDiff(1, prose) + format-corrected retry(1, NEED_FILES) = 3 calls.
	if provider.calls != 3 {
		t.Errorf("provider called %d times, want 3 (picker + prose + recovery)", provider.calls)
	}
}

// TestImplementerRunFailsAfterRepeatedFormatViolations guards the
// retry cap: a model that responds with prose twice in a row (once
// on the initial turn, once on the correction) must fail hard
// rather than loop forever. Format-correction is bounded at 1 retry
// — past that, something is deeply wrong and more retries waste
// tokens without fixing it.
func TestImplementerRunFailsAfterRepeatedFormatViolations(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	provider := &scriptedProvider{
		responses: []string{
			`["x.go"]`, // picker
			"Here's what I think we should do: first, we'd need to check the existing call sites...",
			"Actually, let me think about this differently. The consolidation should...",
		},
	}
	impl := &Implementer{Provider: provider}
	ctx := &Context{
		Issue:    &github.Issue{Owner: "a", Repo: "b", Number: 1, Title: "T", Body: "b"},
		Snapshot: &repo.Snapshot{Root: dir},
	}
	sketch := &Sketch{Number: 1, Title: "t", Markdown: "m"}

	_, err := impl.Run(context.Background(), ctx, sketch)
	if err == nil {
		t.Fatal("expected error after repeated format violations")
	}
	if !strings.Contains(err.Error(), "format-correction attempts") {
		t.Errorf("expected format-correction cap error, got: %v", err)
	}
}

// TestImplementerRunFormatCorrectionPromptQuotesOriginal verifies
// the correction message actually quotes the model's broken
// response back at it. This is what makes the correction effective:
// the model sees exactly what it wrote and can compare it to the
// format rule. We check by using a scripted provider that records
// the last Request it received.
func TestImplementerRunFormatCorrectionPromptQuotesOriginal(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Use a provider that captures the user message on each call.
	captures := &captureAllProvider{
		responses: []string{
			`["x.go"]`,
			"Looking at this, I need the TSX call sites to anchor edits precisely.",
			"diff --git a/x.go b/x.go\n--- a/x.go\n+++ b/x.go\n@@ -1 +1 @@\n-package x\n+package y\n",
		},
	}
	impl := &Implementer{Provider: captures}
	ctx := &Context{
		Issue:    &github.Issue{Owner: "a", Repo: "b", Number: 1, Title: "T", Body: "b"},
		Snapshot: &repo.Snapshot{Root: dir},
	}
	sketch := &Sketch{Number: 1, Title: "t", Markdown: "m"}

	patch, err := impl.Run(context.Background(), ctx, sketch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if patch == nil {
		t.Fatal("nil patch")
	}

	// The third call should be the format-corrected retry — it
	// should contain the broken quote from the second call.
	if len(captures.prompts) < 3 {
		t.Fatalf("want 3 captured prompts, got %d", len(captures.prompts))
	}
	retryPrompt := captures.prompts[2]
	if !strings.Contains(retryPrompt, "YOUR PREVIOUS RESPONSE VIOLATED THE OUTPUT FORMAT") {
		t.Errorf("correction prompt missing violation header; got:\n%s", retryPrompt)
	}
	if !strings.Contains(retryPrompt, "Looking at this, I need the TSX call sites") {
		t.Errorf("correction prompt did not quote the broken response back to the model; got:\n%s", retryPrompt)
	}
}

// captureAllProvider records every user message passed to Complete
// and returns scripted responses in order. Used by
// TestImplementerRunFormatCorrectionPromptQuotesOriginal to verify
// the retry prompt actually quotes the original violation.
type captureAllProvider struct {
	responses []string
	prompts   []string
}

func (p *captureAllProvider) Name() string { return "captureAll" }
func (p *captureAllProvider) Complete(_ context.Context, req llm.Request) (llm.Response, error) {
	if len(req.Messages) > 0 {
		p.prompts = append(p.prompts, req.Messages[0].Content)
	}
	idx := len(p.prompts) - 1
	if idx >= len(p.responses) {
		idx = len(p.responses) - 1
	}
	return llm.Response{Content: p.responses[idx]}, nil
}

// TestImplementerRunSurfacesMissingPathsAndRecovers is the
// regression guard for the real failure the user hit: the picker
// hallucinated plausible-but-nonexistent paths (apps/marketing/src/
// pages/Sectors.tsx) and the harness silently dropped them from
// readFiles, leaving the model with no feedback signal. It kept
// asking for more hallucinated paths until the NEED_FILES cap
// tripped, while the REAL paths sat in the inventory unread.
//
// After this fix, readFiles reports missing paths, Run threads
// them into the next generateDiff turn under a 'DO NOT request
// these again' section, and the model can self-correct on the
// retry. This test drives that exact flow: hallucinated path in
// the picker, real path on the NEED_FILES round, success.
func TestImplementerRunSurfacesMissingPathsAndRecovers(t *testing.T) {
	dir := t.TempDir()
	// Real file that the model SHOULD have asked for from the start.
	realPath := filepath.Join(dir, "apps", "marketing", "src", "components", "pages", "pricing", "pricing.tsx")
	if err := os.MkdirAll(filepath.Dir(realPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(realPath, []byte("export const Pricing = () => <div>hello</div>\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	provider := &captureAllProvider{
		responses: []string{
			// Turn 1 (picker): hallucinated path. This is the real
			// failure mode — the picker guessed a plausible-looking
			// path structure that doesn't match the actual repo.
			`["apps/marketing/src/pages/Pricing.tsx"]`,
			// Turn 2 (generateDiff round 0): model sees the
			// hallucinated path in its missing list and correctly
			// requests the REAL path.
			`NEED_FILES: ["apps/marketing/src/components/pages/pricing/pricing.tsx"]`,
			// Turn 2 (generateDiff round 1): real diff with the
			// loaded file contents.
			"diff --git a/apps/marketing/src/components/pages/pricing/pricing.tsx b/apps/marketing/src/components/pages/pricing/pricing.tsx\n" +
				"--- a/apps/marketing/src/components/pages/pricing/pricing.tsx\n" +
				"+++ b/apps/marketing/src/components/pages/pricing/pricing.tsx\n" +
				"@@ -1 +1 @@\n" +
				"-export const Pricing = () => <div>hello</div>\n" +
				"+export const Pricing = () => <div>contact sales</div>\n",
		},
	}
	impl := &Implementer{Provider: provider}
	ctx := &Context{
		Issue:    &github.Issue{Owner: "a", Repo: "b", Number: 1, Title: "T", Body: "body"},
		Snapshot: &repo.Snapshot{Root: dir},
	}
	sketch := &Sketch{Number: 1, Title: "consolidate CTA", Markdown: "## Plan\n- update pricing CTA"}

	patch, err := impl.Run(context.Background(), ctx, sketch)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if patch == nil {
		t.Fatal("nil patch")
	}
	if !strings.Contains(patch.Diff, "contact sales") {
		t.Errorf("patch missing expected content, got:\n%s", patch.Diff)
	}

	// Verify the second prompt (generateDiff round 0) surfaced the
	// missing path under the DO NOT request again heading. Without
	// that surfacing the model has no feedback signal and the fix
	// is moot.
	if len(provider.prompts) < 2 {
		t.Fatalf("want at least 2 prompts, got %d", len(provider.prompts))
	}
	secondPrompt := provider.prompts[1]
	if !strings.Contains(secondPrompt, "DO NOT EXIST") {
		t.Errorf("second prompt missing 'DO NOT EXIST' missing-paths header; got:\n%s", secondPrompt)
	}
	if !strings.Contains(secondPrompt, "apps/marketing/src/pages/Pricing.tsx") {
		t.Errorf("second prompt should list the hallucinated path as missing; got:\n%s", secondPrompt)
	}
	// The inventory should also appear in every generateDiff turn,
	// not just the picker. This is what gives the model something
	// real to pick from on the retry.
	if !strings.Contains(secondPrompt, "Repository file inventory") {
		t.Errorf("second prompt missing file inventory; got:\n%s", secondPrompt)
	}
}

// TestImplementerRunRejectsRepeatedFileRequests guards against a
// model that keeps re-requesting paths the harness already loaded.
// We de-duplicate inside Run; if the model asks for ONLY files that
// are already in the contents map, the loop short-circuits rather
// than wasting another LLM round on the same context.
func TestImplementerRunRejectsRepeatedFileRequests(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	provider := &scriptedProvider{
		responses: []string{
			`["a.go"]`,            // picker loads a.go
			`NEED_FILES: ["a.go"]`, // model re-requests the same file
		},
	}
	impl := &Implementer{Provider: provider}
	ctx := &Context{
		Issue:    &github.Issue{Owner: "aisu-ai", Repo: "aidev", Number: 1, Title: "T", Body: "b"},
		Snapshot: &repo.Snapshot{Root: dir},
	}
	sketch := &Sketch{Number: 1, Title: "t", Markdown: "m"}

	_, err := impl.Run(context.Background(), ctx, sketch)
	if err == nil {
		t.Fatal("expected error on duplicate-only NEED_FILES request")
	}
	if !strings.Contains(err.Error(), "already provided") {
		t.Errorf("expected 'already provided' error, got: %v", err)
	}
}

// harvestPaths tests — the Implementer's pre-flight path extractor
// that pulls repo-relative source paths out of upstream agent prose
// and loads them automatically before the picker turn. The
// regression guard against 'files don't exist' ERRORs when the
// Clarifier literally just cited the file.

func TestHarvestPathsFromClarifierEvidence(t *testing.T) {
	// The exact shape of a Clarifier evidence bullet — file path
	// followed by a line-number annotation. This is what the user's
	// Wave 1 Clarifier output contained, verbatim.
	input := "In `apps/marketing/src/components/sections/pricing-section/pricing-section-client.tsx:193-199` the Button call site uses t(\"contactSales\")."
	got := harvestPaths(input)
	if len(got) != 1 {
		t.Fatalf("want 1 path, got %d: %v", len(got), got)
	}
	want := "apps/marketing/src/components/sections/pricing-section/pricing-section-client.tsx"
	if got[0] != want {
		t.Errorf("got %q, want %q", got[0], want)
	}
}

func TestHarvestPathsMultipleExtensions(t *testing.T) {
	input := `
The translation lives in apps/marketing/src/lib/i18n/translations/fr/common.json and the
call site is at apps/marketing/src/components/pages/pricing/pricing.tsx:56. The type
definition is in packages/types/src/i18n.ts and the helper is use-translation.ts at
apps/marketing/src/lib/i18n/use-translation.ts.
`
	got := harvestPaths(input)
	want := map[string]bool{
		"apps/marketing/src/lib/i18n/translations/fr/common.json": true,
		"apps/marketing/src/components/pages/pricing/pricing.tsx": true,
		"packages/types/src/i18n.ts":                              true,
		"apps/marketing/src/lib/i18n/use-translation.ts":          true,
	}
	if len(got) != len(want) {
		t.Errorf("got %d paths %v, want %d (%v)", len(got), got, len(want), want)
	}
	for _, p := range got {
		if !want[p] {
			t.Errorf("unexpected path %q", p)
		}
	}
}

func TestHarvestPathsDeduplicates(t *testing.T) {
	input := "foo/bar.go foo/bar.go foo/bar.go"
	got := harvestPaths(input)
	if len(got) != 1 || got[0] != "foo/bar.go" {
		t.Errorf("got %v, want [foo/bar.go]", got)
	}
}

func TestHarvestPathsFromMultipleSources(t *testing.T) {
	scout := "Top-level is apps/marketing/src/app/page.tsx with components/..."
	critic := "Worth checking: apps/marketing/src/components/cta/button.tsx"
	clarifier := "Fix site: apps/marketing/src/components/pages/pricing/pricing.tsx:56"
	got := harvestPaths(scout, critic, clarifier)
	if len(got) != 3 {
		t.Errorf("want 3, got %d: %v", len(got), got)
	}
}

func TestHarvestPathsRejectsAbsoluteAndTraversal(t *testing.T) {
	input := "Check /etc/passwd and ../../../outside.go but foo/bar.go is fine."
	got := harvestPaths(input)
	if len(got) != 1 || got[0] != "foo/bar.go" {
		t.Errorf("got %v, want only [foo/bar.go] (absolute and traversal filtered)", got)
	}
}

func TestHarvestPathsIgnoresBareFilenames(t *testing.T) {
	// Bare filenames without a directory component ("main.go" on its
	// own) are too noisy to harvest — they match too many things in
	// prose. The regex requires at least one slash.
	got := harvestPaths("see main.go for details, or check foo.tsx")
	if len(got) != 0 {
		t.Errorf("bare filenames should be rejected, got %v", got)
	}
}

func TestHarvestPathsIgnoresEmptyInput(t *testing.T) {
	got := harvestPaths("", "   ", "\n\n")
	if len(got) != 0 {
		t.Errorf("empty inputs should return nothing, got %v", got)
	}
}

// TestImplementerRunAutoLoadsUpstreamCitedPaths is the regression
// guard for the 'ERROR: files do not exist' failure the user hit.
// The Clarifier cites a path that is NOT in the picker's inventory
// (simulates an inventory cap truncation). The Implementer.Run()
// must auto-harvest the path from the Clarifier content, load it
// via readFiles, and make it available to generateDiff BEFORE the
// picker runs. Without this fix the Implementer would see only the
// truncated inventory, conclude the file doesn't exist, and ERROR.
func TestImplementerRunAutoLoadsUpstreamCitedPaths(t *testing.T) {
	dir := t.TempDir()
	// Create the "hidden" file in a deep subdirectory — the
	// Clarifier cites it but we'll simulate the picker not seeing
	// it by having the picker return an empty inventory pick.
	citedPath := "apps/marketing/src/components/pages/pricing/pricing.tsx"
	if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(citedPath)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, citedPath), []byte("export const Pricing = () => <>\"Contact our sales team\"</>\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	provider := &captureAllProvider{
		responses: []string{
			// Picker turn: returns empty (simulating the picker not
			// knowing about the cited path because it's truncated
			// out of the inventory). This matches the real failure
			// where the picker returned an empty list too.
			`[]`,
			// generateDiff turn: because Run() auto-harvested the
			// Clarifier path and loaded it, the file contents are
			// already in the prompt. Model produces a real diff.
			"diff --git a/apps/marketing/src/components/pages/pricing/pricing.tsx b/apps/marketing/src/components/pages/pricing/pricing.tsx\n" +
				"--- a/apps/marketing/src/components/pages/pricing/pricing.tsx\n" +
				"+++ b/apps/marketing/src/components/pages/pricing/pricing.tsx\n" +
				"@@ -1 +1 @@\n" +
				"-export const Pricing = () => <>\"Contact our sales team\"</>\n" +
				"+export const Pricing = () => <>\"Contact Sales\"</>\n",
		},
	}
	impl := &Implementer{Provider: provider}
	ctx := &Context{
		Issue: &github.Issue{
			Owner: "a", Repo: "b", Number: 1,
			Title: "Unify CTA",
			Body:  "fix the drift",
		},
		Snapshot: &repo.Snapshot{Root: dir},
		ClarifierNotes: "### q1 — evidence\n\n**Answer:** The call site is at `" + citedPath + ":193-199` and it uses t(\"contactSales\").",
	}
	sketch := &Sketch{Number: 1, Title: "consolidate", Markdown: "## Plan\n- update the " + citedPath + " label"}

	patch, err := impl.Run(context.Background(), ctx, sketch)
	if err != nil {
		t.Fatalf("Run should not fail — upstream-cited path should be auto-loaded: %v", err)
	}
	if patch == nil {
		t.Fatal("nil patch")
	}
	if !strings.Contains(patch.Diff, "Contact Sales") {
		t.Errorf("patch missing expected change, got:\n%s", patch.Diff)
	}

	// Verify the second prompt (generateDiff turn) actually
	// contains the file contents Run() auto-loaded from the
	// Clarifier citation. If the harvest didn't fire, the contents
	// section would be empty.
	if len(provider.prompts) < 2 {
		t.Fatalf("want 2 prompts, got %d", len(provider.prompts))
	}
	diffPrompt := provider.prompts[1]
	if !strings.Contains(diffPrompt, "export const Pricing") {
		t.Errorf("diff-generation prompt did not include auto-harvested file contents; got:\n%s", diffPrompt)
	}
	if !strings.Contains(diffPrompt, citedPath) {
		t.Errorf("diff-generation prompt should cite the upstream path under ## Current file contents; got:\n%s", diffPrompt)
	}
}

func TestParseNeedFilesCapsLength(t *testing.T) {
	// parseNeedFiles reuses cleanFileList's cap, so a huge request
	// still fits within maxRequestedFiles.
	var b []byte
	b = append(b, []byte(`NEED_FILES: [`)...)
	for j := 0; j < maxRequestedFiles*3; j++ {
		if j > 0 {
			b = append(b, ',')
		}
		b = append(b, []byte(`"f`+intoa(j)+`.go"`)...)
	}
	b = append(b, ']')
	got, err := parseNeedFiles(string(b))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != maxRequestedFiles {
		t.Errorf("got %d, want cap at %d", len(got), maxRequestedFiles)
	}
}
