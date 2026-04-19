package orchestrator

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/aisu-ai/aidev/internal/agents"
	"github.com/aisu-ai/aidev/internal/github"
)

// TestSaveLoadTriageStateRoundtrip is the load-bearing regression test
// for issue #53: a Save + fresh-Load must restore enough of
// agents.Context that the Triage agent can run end-to-end (Issue +
// ScoutReport + CriticReport + Sketches + Selector populated).
//
// The Triage agent's entry validation requires:
//   c.Issue != nil
//   c.Selector != nil && c.Selector.ChosenNumber > 0
//   c.Selector.ChosenNumber <= len(c.Sketches)
// so those fields are the invariants we assert post-load.
func TestSaveLoadTriageStateRoundtrip(t *testing.T) {
	dir := t.TempDir()

	originalIssue := &github.Issue{
		Owner: "AiSU-AI", Repo: "aidev", Number: 53,
		Title: "fix: triage hydration", Body: "issue body",
	}
	originalSketches := []agents.Sketch{
		{Number: 1, Title: "Sketch A", Markdown: "## Sketch 1: Sketch A\n\nbody A"},
		{Number: 2, Title: "Sketch B", Markdown: "## Sketch 2: Sketch B\n\nbody B"},
	}
	originalVerdict := &agents.SelectorVerdict{
		ChosenNumber:  1,
		Score:         4.5,
		Rationale:     "picked A",
		TieBreakerUsed: false,
		RubricVersion: "1.0",
	}

	oSave := &Orchestrator{
		ctx: &agents.Context{
			Issue:        originalIssue,
			ScoutReport:  "scout body",
			CriticReport: "critic body",
			Sketches:     originalSketches,
			Selector:     originalVerdict,
		},
	}
	if err := oSave.SaveTriageState(dir); err != nil {
		t.Fatalf("SaveTriageState: %v", err)
	}

	// Confirm sidecar exists at the expected path.
	sidecarPath := filepath.Join(dir, triageStateFilename)
	if _, err := os.Stat(sidecarPath); err != nil {
		t.Fatalf("sidecar not present after save: %v", err)
	}

	// Fresh orchestrator — simulates `aidev review -triage` running
	// in a separate process with no in-memory state.
	oLoad := &Orchestrator{ctx: &agents.Context{}}
	if err := oLoad.LoadTriageState(dir); err != nil {
		t.Fatalf("LoadTriageState: %v", err)
	}

	// Every Triage-required invariant must hold after load.
	if oLoad.ctx.Issue == nil || oLoad.ctx.Issue.Number != 53 {
		t.Errorf("Issue not restored: %+v", oLoad.ctx.Issue)
	}
	if oLoad.ctx.ScoutReport != "scout body" {
		t.Errorf("ScoutReport lost: %q", oLoad.ctx.ScoutReport)
	}
	if oLoad.ctx.CriticReport != "critic body" {
		t.Errorf("CriticReport lost: %q", oLoad.ctx.CriticReport)
	}
	if len(oLoad.ctx.Sketches) != 2 {
		t.Fatalf("expected 2 sketches, got %d", len(oLoad.ctx.Sketches))
	}
	if oLoad.ctx.Sketches[0].Title != "Sketch A" {
		t.Errorf("Sketch 0 title lost: %q", oLoad.ctx.Sketches[0].Title)
	}
	if oLoad.ctx.Selector == nil || oLoad.ctx.Selector.ChosenNumber != 1 {
		t.Errorf("Selector not restored: %+v", oLoad.ctx.Selector)
	}
	if oLoad.ctx.Selector.Rationale != "picked A" {
		t.Errorf("Selector rationale lost: %q", oLoad.ctx.Selector.Rationale)
	}
}

// TestLoadTriageStateMissingSidecarReturnsNotExist verifies the
// explicit "no prior state" path — LoadTriageState on a fresh dir
// (no sidecar written) returns an fs.ErrNotExist-compatible error so
// callers can distinguish "first run, hydration skipped" from
// "hydration attempted but corrupt."
func TestLoadTriageStateMissingSidecarReturnsNotExist(t *testing.T) {
	dir := t.TempDir()
	o := &Orchestrator{ctx: &agents.Context{}}
	err := o.LoadTriageState(dir)
	if err == nil {
		t.Fatal("expected error for missing sidecar, got nil")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected fs.ErrNotExist, got %v (type %T)", err, err)
	}
}

// TestLoadTriageStateRejectsPartialHydration is the sharp-question-#2
// regression: a sidecar that exists but is missing the Selector
// verdict (or has ChosenNumber=0) must fail loudly. Silently half-
// populating the Context would put Triage in a worse state than
// starting fresh.
func TestLoadTriageStateRejectsPartialHydration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, triageStateFilename)

	// Scenario 1: Selector is nil.
	if err := os.WriteFile(path, []byte(`{"issue":{"Owner":"a","Repo":"b","Number":1},"selector":null,"sketches":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	o := &Orchestrator{ctx: &agents.Context{}}
	if err := o.LoadTriageState(dir); err == nil {
		t.Error("expected error for nil Selector, got nil")
	}

	// Scenario 2: Selector present but ChosenNumber=0 (Selector refused to pick).
	if err := os.WriteFile(path, []byte(`{"issue":{"Owner":"a","Repo":"b","Number":1},"selector":{"chosen_number":0,"rationale":"refused"},"sketches":[{"Number":1,"Title":"t","Markdown":"m"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	o = &Orchestrator{ctx: &agents.Context{}}
	if err := o.LoadTriageState(dir); err == nil {
		t.Error("expected error for ChosenNumber=0, got nil")
	}

	// Scenario 3: Selector has a pick but sketches array is empty.
	if err := os.WriteFile(path, []byte(`{"issue":{"Owner":"a","Repo":"b","Number":1},"selector":{"chosen_number":1,"rationale":"r"},"sketches":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	o = &Orchestrator{ctx: &agents.Context{}}
	if err := o.LoadTriageState(dir); err == nil {
		t.Error("expected error for empty sketches, got nil")
	}
}

// TestLoadTriageStateRejectsMalformedJSON — ensure parse failures
// surface loudly rather than silently zeroing the Context.
func TestLoadTriageStateRejectsMalformedJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, triageStateFilename)
	if err := os.WriteFile(path, []byte(`not valid json at all`), 0o644); err != nil {
		t.Fatal(err)
	}
	o := &Orchestrator{ctx: &agents.Context{}}
	err := o.LoadTriageState(dir)
	if err == nil {
		t.Fatal("expected error for malformed JSON, got nil")
	}
}

// TestSaveTriageStateRefusesEmptyRunDir and TestLoadTriageStateRefusesEmptyRunDir
// — both save/load require a non-empty runDir. Passing "" is a caller
// bug (LoadIssue wasn't called, or runDir resolution failed); surface
// it rather than silently writing to cwd or reading a random file.
func TestSaveTriageStateRefusesEmptyRunDir(t *testing.T) {
	o := &Orchestrator{ctx: &agents.Context{}}
	if err := o.SaveTriageState(""); err == nil {
		t.Error("expected error for empty runDir, got nil")
	}
}

func TestLoadTriageStateRefusesEmptyRunDir(t *testing.T) {
	o := &Orchestrator{ctx: &agents.Context{}}
	if err := o.LoadTriageState(""); err == nil {
		t.Error("expected error for empty runDir, got nil")
	}
}
