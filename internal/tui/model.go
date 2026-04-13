// Package tui is the Bubble Tea application that drives aidev interactively.
//
// The model is intentionally small: it holds the orchestrator, the current
// event stream, and which pane is focused. All domain work happens inside
// the orchestrator; the TUI just pumps events into viewport updates.
package tui

import (
	"context"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/aisu-ai/aidev/internal/orchestrator"
)

// pane identifies the currently focused viewport so Tab can cycle between
// them. The third pane doubles as the critic pane in the first half of the
// pipeline and the "current output" pane later — it retitles and rewrites
// its content as each downstream phase (critic → architect → implementer →
// tester → reviewer) produces new output.
type pane int

const (
	paneIssue pane = iota
	paneScout
	paneOutput
	numPanes
)

// phase tracks which half of the pipeline the TUI is currently driving. It
// is derived from orchestrator state but cached on the model so the view
// layer doesn't have to call into the orchestrator for every render.
type phase int

const (
	phaseIdle        phase = iota // waiting for user to press 'r'
	phaseRunning                  // scout or critic is mid-flight
	phaseAwaitUser                // critic done, waiting for a/k
	phaseArchitect                // architect is mid-flight
	phaseSketches                 // sketches ready for review — 1-9 to pick one
	phaseImplementing             // implementer mid-flight
	phasePatchReady               // patch ready — t to test, v to review
	phaseTesting                  // tester mid-flight
	phaseTestsDone                // tests passed or failed — v to review
	phaseReviewing                // reviewer mid-flight
	phaseReviewDone               // review complete — q to finish
	phaseKilled                   // user killed the proposal
	phaseErrored                  // something broke
)

// Model is the root Bubble Tea model.
type Model struct {
	orch     *orchestrator.Orchestrator
	issueURL string
	repoRoot string

	issueVP  viewport.Model
	scoutVP  viewport.Model
	outputVP viewport.Model

	focus   pane
	keys    keyMap
	width   int
	height  int
	ready   bool
	status  string
	lastErr error

	// phase caches the high-level TUI state so view() can branch without
	// reaching into the orchestrator on every render.
	phase phase

	// outputTitle is the current label on the rightmost pane. It flips
	// through "Critic report" → "Architect sketches" → "Implementer
	// patch" → "Test result" → "Review" as the pipeline advances.
	outputTitle string

	// events is the current active event stream; only one phase drives it
	// at a time (Run, Continue, Implement, Test, or ReviewPatch).
	events <-chan orchestrator.Event
}

// New constructs a Model ready to be passed to tea.NewProgram.
func New(o *orchestrator.Orchestrator, issueURL, repoRoot string) Model {
	preview := o.CostPreview()
	return Model{
		orch:        o,
		issueURL:    issueURL,
		repoRoot:    repoRoot,
		keys:        defaultKeys(),
		phase:       phaseIdle,
		outputTitle: "Critic report",
		status:      formatIdleStatus(preview.N, preview.TotalOutputTokens, preview.EstimatedSeconds),
	}
}

// Init performs startup work. We pre-populate the issue viewport with the
// issue body the caller already fetched via orchestrator.LoadIssue.
func (m Model) Init() tea.Cmd {
	return nil
}

// runPipelineCmd kicks off the first-pass pipeline (scout -> critic -> await)
// and returns a command that awaits the first event.
func (m *Model) runPipelineCmd() tea.Cmd {
	m.events = m.orch.Run(context.Background())
	m.phase = phaseRunning
	return waitForEvent(m.events)
}

// continuePipelineCmd kicks off the second-pass pipeline (architect) after
// the user has approved the Critic's recommendation.
func (m *Model) continuePipelineCmd() tea.Cmd {
	m.events = m.orch.Continue(context.Background())
	m.phase = phaseArchitect
	return waitForEvent(m.events)
}

// implementCmd kicks off the Implementer on the given sketch number.
func (m *Model) implementCmd(n int) tea.Cmd {
	m.events = m.orch.Implement(context.Background(), n)
	m.phase = phaseImplementing
	return waitForEvent(m.events)
}

// testCmd kicks off the Tester against the currently-loaded repo.
func (m *Model) testCmd() tea.Cmd {
	m.events = m.orch.Test(context.Background())
	m.phase = phaseTesting
	return waitForEvent(m.events)
}

// reviewCmd kicks off the Reviewer against the current patch.
func (m *Model) reviewCmd() tea.Cmd {
	patch := ""
	if p := m.orch.Patch(); p != nil {
		patch = p.Diff
	}
	if patch == "" {
		return nil
	}
	m.events = m.orch.ReviewPatch(context.Background(), patch)
	m.phase = phaseReviewing
	return waitForEvent(m.events)
}

// eventMsg wraps an orchestrator.Event so it can travel through the Bubble
// Tea message pipeline.
type eventMsg struct{ ev orchestrator.Event }

// eventsClosedMsg marks the end of the current event stream.
type eventsClosedMsg struct{}

func waitForEvent(ch <-chan orchestrator.Event) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return eventsClosedMsg{}
		}
		return eventMsg{ev: ev}
	}
}
