package orchestrator

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/aisu-ai/aidev/internal/agents"
	"github.com/aisu-ai/aidev/internal/github"
)

// triageStateFilename is the fixed filename under the runDir that holds
// a JSON snapshot of the subset of agents.Context the Triage agent
// needs. Lives alongside architect-output.md; the JSON is the
// machine-readable sidecar so `aidev review -triage` doesn't have to
// parse Markdown to hydrate state.
const triageStateFilename = "triage-state.json"

// triageState is the minimal serializable projection of agents.Context
// needed to hydrate a fresh orchestrator for Triage. We deliberately
// don't serialize the full Context because most of its fields
// (Snapshot, ClarifierNotes, CoordinatorFeedback) are either
// recomputed from disk per-run or unused by Triage. Tight scope keeps
// the on-disk format forward-compatible.
//
// Issue #53 motivation: without this, `aidev review -triage` runs in a
// fresh process, constructs an empty agents.Context, and the Triage
// agent refuses with "no chosen sketch — Selector verdict required".
// Reading this sidecar restores the Selector + sketches so the Triage
// judgment can actually run.
type triageState struct {
	Issue        *github.Issue           `json:"issue"`
	ScoutReport  string                  `json:"scout_report"`
	CriticReport string                  `json:"critic_report"`
	Principles   []agents.Principle      `json:"principles,omitempty"`
	Sketches     []agents.Sketch         `json:"sketches"`
	Selector     *agents.SelectorVerdict `json:"selector"`
}

// SaveTriageState writes a JSON snapshot of the orchestrator's
// Triage-relevant state to `<runDir>/triage-state.json`. Call it from
// the autonomous-run driver AFTER the pipeline has produced a Selector
// verdict (i.e. same trigger as writeArchitectOutput).
//
// Best-effort by design — returns the underlying I/O error so the
// caller can log it, but the pipeline should not fail just because
// the sidecar couldn't be written. Triage will fall back to the
// existing "no chosen sketch" error on the next run, which is loud
// and recoverable.
func (o *Orchestrator) SaveTriageState(runDir string) error {
	if runDir == "" {
		return fmt.Errorf("orchestrator: SaveTriageState needs a non-empty runDir")
	}
	if o.ctx == nil {
		return fmt.Errorf("orchestrator: cannot save triage state before LoadIssue")
	}
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return fmt.Errorf("orchestrator: mkdir %s: %w", runDir, err)
	}
	state := triageState{
		Issue:        o.ctx.Issue,
		ScoutReport:  o.ctx.ScoutReport,
		CriticReport: o.ctx.CriticReport,
		Principles:   o.ctx.Principles,
		Sketches:     o.ctx.Sketches,
		Selector:     o.ctx.Selector,
	}
	data, err := json.MarshalIndent(&state, "", "  ")
	if err != nil {
		return fmt.Errorf("orchestrator: marshal triage state: %w", err)
	}
	path := filepath.Join(runDir, triageStateFilename)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("orchestrator: write %s: %w", path, err)
	}
	return nil
}

// LoadTriageState reads the sidecar produced by SaveTriageState and
// hydrates orchestrator.ctx with the decoded fields. Call from the
// `aidev review -triage` path before invoking Triage so the chosen
// Sketch + Selector verdict are present.
//
// Contract is all-or-nothing per the issue's sharp-question #2: a
// partially-parseable sidecar returns an error (loud failure) rather
// than a half-populated Context. Returns fs.ErrNotExist wrapped if
// the sidecar isn't there so callers can distinguish "no prior state"
// from "corrupted state" with errors.Is.
func (o *Orchestrator) LoadTriageState(runDir string) error {
	if runDir == "" {
		return fmt.Errorf("orchestrator: LoadTriageState needs a non-empty runDir")
	}
	if o.ctx == nil {
		return fmt.Errorf("orchestrator: cannot load triage state before LoadIssue")
	}
	path := filepath.Join(runDir, triageStateFilename)
	data, err := os.ReadFile(path)
	if err != nil {
		// Pass through fs.ErrNotExist unwrapped so errors.Is works.
		return err
	}
	var state triageState
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("orchestrator: parse triage state %s: %w", path, err)
	}
	// All-or-nothing hydration: if Selector is nil or ChosenNumber is
	// zero (no valid pick), treat as unusable. The caller's existing
	// "no chosen sketch" branch kicks in naturally — we just make sure
	// we don't half-populate the Context and leave it in a worse state
	// than it started.
	if state.Selector == nil || state.Selector.ChosenNumber == 0 {
		return fmt.Errorf("orchestrator: triage state at %s has no usable Selector verdict", path)
	}
	if len(state.Sketches) == 0 {
		return fmt.Errorf("orchestrator: triage state at %s has no sketches (Selector verdict would reference non-existent sketch)", path)
	}
	o.ctx.Issue = state.Issue
	o.ctx.ScoutReport = state.ScoutReport
	o.ctx.CriticReport = state.CriticReport
	o.ctx.Principles = state.Principles
	o.ctx.Sketches = state.Sketches
	o.ctx.Selector = state.Selector
	return nil
}
