package orchestrator

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aisu-ai/aidev/internal/agents"
	"github.com/aisu-ai/aidev/internal/github"
	"github.com/aisu-ai/aidev/internal/llm"
	"github.com/aisu-ai/aidev/internal/repo"
)

// scriptedProvider replays a fixed sequence of canned responses
// across successive Complete() calls. Used by the Gate-1 loop tests
// to drive the Implementer ↔ Coordinator back-and-forth without
// hitting a real LLM. Not exported — these helpers are deliberately
// duplicated rather than shared via a testutil package so the public
// API surface stays minimal.
type scriptedProvider struct {
	name      string
	responses []string
	calls     int
}

func (p *scriptedProvider) Name() string { return p.name }
func (p *scriptedProvider) Complete(_ context.Context, _ llm.Request) (llm.Response, error) {
	idx := p.calls
	if idx >= len(p.responses) {
		idx = len(p.responses) - 1
	}
	p.calls++
	return llm.Response{Content: p.responses[idx]}, nil
}

func newTestOrchestrator(t *testing.T, dir string, implProvider, coordProvider llm.Provider) *Orchestrator {
	t.Helper()
	return &Orchestrator{
		state: StateSketchesReady,
		ctx: &agents.Context{
			Issue: &github.Issue{
				Owner: "aisu-ai", Repo: "aidev", Number: 1,
				Title: "T", Body: "test body",
			},
			Snapshot: &repo.Snapshot{Root: dir},
			Sketches: []agents.Sketch{
				{Number: 1, Title: "test sketch", Markdown: "## Plan\n- do the thing"},
			},
		},
		implementer: &agents.Implementer{Provider: implProvider},
		coordinator: &agents.Coordinator{Provider: coordProvider},
		reporterLog: io.Discard,
	}
}

// TestImplementGateCoordinatorApprovesFirstRun drives the happy
// path: Implementer produces a clean diff, Coordinator approves on
// the first pass, the patch is written to disk and patch_ready is
// emitted. No retries.
func TestImplementGateCoordinatorApprovesFirstRun(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	impl := &scriptedProvider{
		name: "impl",
		responses: []string{
			`["x.go"]`, // picker
			"diff --git a/x.go b/x.go\n--- a/x.go\n+++ b/x.go\n@@ -1 +1 @@\n-package x\n+package y\n",
		},
	}
	coord := &scriptedProvider{
		name: "coord",
		responses: []string{
			"APPROVED: minimal rename, correct.\n",
		},
	}
	o := newTestOrchestrator(t, dir, impl, coord)

	var gotPatchReady bool
	for ev := range o.Implement(context.Background(), 1) {
		if ev.Err != nil {
			t.Fatalf("unexpected error: %v", ev.Err)
		}
		if ev.State == StatePatchReady {
			gotPatchReady = true
			if !strings.Contains(ev.Message, "APPROVED") {
				t.Errorf("expected APPROVED in message, got %q", ev.Message)
			}
		}
	}
	if !gotPatchReady {
		t.Fatal("never reached patch_ready")
	}
	if o.Patch() == nil {
		t.Fatal("orchestrator.Patch() nil after approval")
	}
	if o.CoordinatorReview() == nil || !o.CoordinatorReview().Approved {
		t.Error("CoordinatorReview should be set and approved")
	}
	// Implementer ran exactly once (picker + diff = 2 calls); no retry.
	if impl.calls != 2 {
		t.Errorf("implementer called %d times, want 2 (picker + diff, no retry)", impl.calls)
	}
	if coord.calls != 1 {
		t.Errorf("coordinator called %d times, want 1", coord.calls)
	}
}

// TestImplementGateRetryWithFeedback drives the interesting case:
// Implementer produces a diff, Coordinator flags concerns, feedback
// goes back into Context.CoordinatorFeedback, Implementer produces
// a better diff on the retry, Coordinator approves. Asserts the
// retry actually happened and the final patch is the second
// Implementer's output.
func TestImplementGateRetryWithFeedback(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	impl := &scriptedProvider{
		name: "impl",
		responses: []string{
			// Attempt 1: picker + bad diff (placeholder value)
			`["x.go"]`,
			"diff --git a/x.go b/x.go\n--- a/x.go\n+++ b/x.go\n@@ -1 +1 @@\n-package x\n+package PLACEHOLDER\n",
			// Attempt 2: picker + corrected diff
			`["x.go"]`,
			"diff --git a/x.go b/x.go\n--- a/x.go\n+++ b/x.go\n@@ -1 +1 @@\n-package x\n+package y\n",
		},
	}
	coord := &scriptedProvider{
		name: "coord",
		responses: []string{
			"CONCERNS: PLACEHOLDER is fabricated.\n\n- x.go: 'package PLACEHOLDER' is a fabricated identifier. Use the real package name 'y' per the sketch.\n",
			"APPROVED: corrected to 'package y' as directed.\n",
		},
	}
	o := newTestOrchestrator(t, dir, impl, coord)

	var messages []string
	for ev := range o.Implement(context.Background(), 1) {
		if ev.Err != nil {
			t.Fatalf("unexpected error: %v", ev.Err)
		}
		if ev.Message != "" {
			messages = append(messages, ev.Message)
		}
	}

	if impl.calls != 4 {
		t.Errorf("implementer called %d times, want 4 (2 attempts × picker+diff)", impl.calls)
	}
	if coord.calls != 2 {
		t.Errorf("coordinator called %d times, want 2 (one CONCERNS + one APPROVED)", coord.calls)
	}
	p := o.Patch()
	if p == nil {
		t.Fatal("nil patch after successful retry")
	}
	if !strings.Contains(p.Diff, "+package y") {
		t.Errorf("final patch should contain corrected 'package y', got:\n%s", p.Diff)
	}
	if strings.Contains(p.Diff, "PLACEHOLDER") {
		t.Errorf("final patch still contains PLACEHOLDER; retry didn't override first attempt")
	}
	// Check that the orchestrator emitted a "re-running with feedback" message.
	var sawRetry bool
	for _, m := range messages {
		if strings.Contains(m, "re-running Implementer with feedback") {
			sawRetry = true
		}
	}
	if !sawRetry {
		t.Errorf("expected a 're-running Implementer with feedback' event; got messages:\n%s", strings.Join(messages, "\n---\n"))
	}
}

// TestImplementGateCapReachedAdvisoryFallthrough drives the cap
// path: Coordinator rejects every attempt up to the retry cap. The
// orchestrator must still write the final patch (teammate mode, not
// gatekeeper mode) and emit patch_ready with an advisory message
// that surfaces the Coordinator's concerns.
func TestImplementGateCapReachedAdvisoryFallthrough(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// maxCoordinatorRounds + 1 attempts total.
	mkAttempt := func() []string {
		return []string{
			`["x.go"]`,
			"diff --git a/x.go b/x.go\n--- a/x.go\n+++ b/x.go\n@@ -1 +1 @@\n-package x\n+package still_wrong\n",
		}
	}
	var implScript []string
	for i := 0; i <= maxCoordinatorRounds; i++ {
		implScript = append(implScript, mkAttempt()...)
	}
	impl := &scriptedProvider{name: "impl", responses: implScript}

	var coordScript []string
	for i := 0; i <= maxCoordinatorRounds; i++ {
		coordScript = append(coordScript, "CONCERNS: still wrong.\n\n- x.go: still not right, try again\n")
	}
	coord := &scriptedProvider{name: "coord", responses: coordScript}

	o := newTestOrchestrator(t, dir, impl, coord)

	var finalMsg string
	for ev := range o.Implement(context.Background(), 1) {
		if ev.Err != nil {
			t.Fatalf("unexpected error: %v", ev.Err)
		}
		if ev.State == StatePatchReady {
			finalMsg = ev.Message
		}
	}

	if o.Patch() == nil {
		t.Fatal("patch should still be written in advisory mode, got nil")
	}
	if o.CoordinatorReview() == nil || o.CoordinatorReview().Approved {
		t.Error("coordReview should be set and NOT approved after cap")
	}
	if !strings.Contains(finalMsg, "Coordinator still has concerns") {
		t.Errorf("final patch_ready message should advise on Coordinator concerns; got: %s", finalMsg)
	}
	expectedImplCalls := (maxCoordinatorRounds + 1) * 2 // picker+diff per attempt
	if impl.calls != expectedImplCalls {
		t.Errorf("implementer called %d times, want %d (%d attempts × picker+diff)", impl.calls, expectedImplCalls, maxCoordinatorRounds+1)
	}
}

// TestImplementGateCoordinatorErrorIsNonFatal verifies the safety
// net: if the Coordinator's own Provider errors (e.g. rate limit,
// network drop), we log and proceed as if approved. The Coordinator
// is a safety net, not a hard gate — a broken cloud connection must
// never block the user's patch.
func TestImplementGateCoordinatorErrorIsNonFatal(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	impl := &scriptedProvider{
		name: "impl",
		responses: []string{
			`["x.go"]`,
			"diff --git a/x.go b/x.go\n--- a/x.go\n+++ b/x.go\n@@ -1 +1 @@\n-package x\n+package y\n",
		},
	}
	coord := &errorProvider{err: "simulated rate limit"}
	o := newTestOrchestrator(t, dir, impl, coord)

	var gotPatchReady bool
	for ev := range o.Implement(context.Background(), 1) {
		if ev.Err != nil {
			t.Fatalf("Coordinator error should be non-fatal, got: %v", ev.Err)
		}
		if ev.State == StatePatchReady {
			gotPatchReady = true
		}
	}
	if !gotPatchReady {
		t.Fatal("patch_ready should still fire when Coordinator errors")
	}
	if o.Patch() == nil {
		t.Fatal("patch should still be produced")
	}
	if o.CoordinatorReview() != nil {
		t.Error("CoordinatorReview should be nil when Coordinator provider errored")
	}
}

type errorProvider struct {
	err string
}

func (p *errorProvider) Name() string { return "errorProvider" }
func (p *errorProvider) Complete(_ context.Context, _ llm.Request) (llm.Response, error) {
	return llm.Response{}, &providerError{msg: p.err}
}

type providerError struct{ msg string }

func (e *providerError) Error() string { return e.msg }
