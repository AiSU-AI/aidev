package orchestrator

import (
	"context"
	"testing"

	"github.com/aisu-ai/aidev/internal/agents"
)

// TestRecritiqueRefusesFromWrongState verifies that Recritique is a
// strict state-machine transition from StateAwaitUser. If a caller
// somehow invokes it mid-scout or after a kill, we must fail loudly
// rather than silently re-running the Critic at the wrong point in
// the pipeline.
func TestRecritiqueRefusesFromWrongState(t *testing.T) {
	cases := []State{StateInit, StateScouting, StateCritiquing, StateArchitecting, StateSketchesReady, StateDone, StateKilled}
	for _, s := range cases {
		o := &Orchestrator{state: s, ctx: &agents.Context{}}
		ch := o.Recritique(context.Background())
		var saw bool
		for ev := range ch {
			if ev.Err != nil && ev.State == StateError {
				saw = true
			}
		}
		if !saw {
			t.Errorf("state=%q: expected Recritique to emit an error event", s)
		}
		if o.state != StateError {
			t.Errorf("state=%q: expected Orchestrator.state=error after rejected Recritique, got %q", s, o.state)
		}
	}
}

// TestRecritiqueRequiresPriorScoutReport is the sanity guard for the
// normal state (await_user) but with an empty agent context: even
// though the state is right, there's nothing to critique. The method
// should error, not crash.
func TestRecritiqueRequiresPriorScoutReport(t *testing.T) {
	o := &Orchestrator{
		state: StateAwaitUser,
		ctx:   &agents.Context{}, // no ScoutReport
	}
	ch := o.Recritique(context.Background())
	var gotErr bool
	for ev := range ch {
		if ev.Err != nil {
			gotErr = true
		}
	}
	if !gotErr {
		t.Error("expected Recritique to error when ScoutReport is empty")
	}
}

