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
// them.
type pane int

const (
	paneIssue pane = iota
	paneScout
	paneCritic
	numPanes
)

// Model is the root Bubble Tea model.
type Model struct {
	orch         *orchestrator.Orchestrator
	issueURL     string
	repoRoot     string
	issueVP      viewport.Model
	scoutVP      viewport.Model
	criticVP     viewport.Model
	focus        pane
	keys         keyMap
	width        int
	height       int
	ready        bool
	status       string
	lastErr      error
	pipelineDone bool
	events       <-chan orchestrator.Event
}

// New constructs a Model ready to be passed to tea.NewProgram.
func New(o *orchestrator.Orchestrator, issueURL, repoRoot string) Model {
	return Model{
		orch:     o,
		issueURL: issueURL,
		repoRoot: repoRoot,
		keys:     defaultKeys(),
		status:   "Press 'r' to run Scout + Critic on the loaded issue.",
	}
}

// Init performs startup work. We pre-populate the issue viewport with the
// issue body the caller already fetched via orchestrator.LoadIssue.
func (m Model) Init() tea.Cmd {
	return nil
}

// runPipelineCmd kicks off the orchestrator pipeline and returns a command
// that awaits the first event. Subsequent events are pulled lazily as they
// arrive.
func (m *Model) runPipelineCmd() tea.Cmd {
	// A per-run context; the TUI's Quit path will let the goroutine finish
	// naturally because each Complete() call has its own HTTP timeout.
	m.events = m.orch.Run(context.Background())
	return waitForEvent(m.events)
}

// eventMsg wraps an orchestrator.Event so it can travel through the Bubble
// Tea message pipeline.
type eventMsg struct{ ev orchestrator.Event }

// eventsClosedMsg marks the end of the event stream.
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
