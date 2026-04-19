package orchestrator

import (
	"context"
	"strings"
	"testing"

	"github.com/aisu-ai/aidev/internal/agents"
)

// TestMarkNeedsRefinementFromAwaitUser verifies the happy path: a
// headless auto-run that ended on the natural StateAwaitUser resting
// place (after Critic=unclear with no Clarifier possible) can be
// marked into StateNeedsRefinement by the caller.
//
// The emitted event must carry the supplied reason so the
// GitHubReporter's postNeedsRefinement path has context to include
// in the structured 🤔 audit comment.
//
// This is the regression test that closes issue #46 — without
// MarkNeedsRefinement, callers in cmd/aidev.runHeadless had no clean
// way to promote an `unclear` verdict to the structured refinement
// comment, and silently landed on the vague `aidev:awaiting-decision`
// label instead.
func TestMarkNeedsRefinementFromAwaitUser(t *testing.T) {
	o := &Orchestrator{
		state:    StateAwaitUser,
		ctx:      &agents.Context{},
		reporter: NullReporter{},
	}
	reason := "Critic verdict: unclear. See the Critic report above."

	ch := o.MarkNeedsRefinement(context.Background(), reason)

	var events []Event
	for ev := range ch {
		events = append(events, ev)
	}
	if len(events) != 1 {
		t.Fatalf("expected exactly 1 event, got %d", len(events))
	}
	ev := events[0]
	if ev.Err != nil {
		t.Errorf("expected no error, got %v", ev.Err)
	}
	if ev.State != StateNeedsRefinement {
		t.Errorf("expected State=%q, got %q", StateNeedsRefinement, ev.State)
	}
	if !strings.Contains(ev.Message, reason) {
		t.Errorf("expected Message to contain reason %q, got %q", reason, ev.Message)
	}
	if o.state != StateNeedsRefinement {
		t.Errorf("expected orchestrator.state=%q after MarkNeedsRefinement, got %q",
			StateNeedsRefinement, o.state)
	}
}

// TestMarkNeedsRefinementRefusesFromWrongState mirrors the
// TestRecritiqueRefusesFromWrongState pattern: calling
// MarkNeedsRefinement from any state other than StateAwaitUser is a
// misuse and must fail loudly rather than corrupt the audit trail.
// Specifically we care about:
//
//   - StateInit: before the first Run()
//   - StateScouting / StateCritiquing / StateArchitecting: mid-pipeline
//   - StateSketchesReady: after Architect but before Selector
//   - StateDone / StateKilled / StateNeedsRefinement: terminal states
//
// For every non-await_user state, we expect a StateError emission and
// the orchestrator state pinned to StateError.
func TestMarkNeedsRefinementRefusesFromWrongState(t *testing.T) {
	cases := []State{
		StateInit,
		StateScouting,
		StateCritiquing,
		StateArchitecting,
		StateSketchesReady,
		StateDone,
		StateKilled,
		StateNeedsRefinement, // already in the target state — still rejected
	}
	for _, s := range cases {
		t.Run(string(s), func(t *testing.T) {
			o := &Orchestrator{state: s, ctx: &agents.Context{}, reporter: NullReporter{}}
			ch := o.MarkNeedsRefinement(context.Background(), "test reason")
			var sawErr bool
			for ev := range ch {
				if ev.Err != nil && ev.State == StateError {
					sawErr = true
				}
			}
			if !sawErr {
				t.Errorf("expected MarkNeedsRefinement to emit an error event from state %q", s)
			}
			if o.state != StateError {
				t.Errorf("expected orchestrator.state=error after rejected MarkNeedsRefinement, got %q", o.state)
			}
		})
	}
}
