package tui

import (
	"fmt"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/aisu-ai/aidev/internal/orchestrator"
)

// Update is the Bubble Tea event loop. It intentionally does no domain
// work — every branch either updates a viewport, cycles focus, or asks the
// orchestrator to do something.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd

	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		if !m.ready {
			m.issueVP = viewport.New(msg.Width/3, msg.Height-6)
			m.scoutVP = viewport.New(msg.Width/3, msg.Height-6)
			m.criticVP = viewport.New(msg.Width-2*(msg.Width/3), msg.Height-6)
			m.ready = true
			m.hydrateInitialContent()
		} else {
			m.issueVP.Width = msg.Width / 3
			m.scoutVP.Width = msg.Width / 3
			m.criticVP.Width = msg.Width - 2*(msg.Width/3)
			m.issueVP.Height = msg.Height - 6
			m.scoutVP.Height = msg.Height - 6
			m.criticVP.Height = msg.Height - 6
		}

	case tea.KeyMsg:
		switch {
		case key.Matches(msg, m.keys.Quit):
			return m, tea.Quit
		case key.Matches(msg, m.keys.Tab):
			m.focus = (m.focus + 1) % numPanes
		case key.Matches(msg, m.keys.Run):
			if m.events == nil {
				m.status = "Running Scout..."
				cmd = m.runPipelineCmd()
				return m, cmd
			}
		case key.Matches(msg, m.keys.Approve):
			if m.pipelineDone {
				m.status = "Approved: the Architect agent will take over in the next milestone."
			}
		case key.Matches(msg, m.keys.Kill):
			if m.pipelineDone {
				m.status = "Killed. No implementation will proceed."
			}
		}

	case eventMsg:
		m.applyEvent(msg.ev)
		cmd = waitForEvent(m.events)
		return m, cmd

	case eventsClosedMsg:
		m.pipelineDone = true
		m.events = nil
	}

	// Forward to the focused viewport so the user can scroll.
	switch m.focus {
	case paneIssue:
		m.issueVP, cmd = m.issueVP.Update(msg)
	case paneScout:
		m.scoutVP, cmd = m.scoutVP.Update(msg)
	case paneCritic:
		m.criticVP, cmd = m.criticVP.Update(msg)
	}
	return m, cmd
}

// hydrateInitialContent fills the issue viewport with the already-loaded
// issue and leaves the scout/critic panes empty until the pipeline runs.
func (m *Model) hydrateInitialContent() {
	ctx := m.orch.AgentContext()
	if ctx.Issue != nil {
		body := fmt.Sprintf("# %s/%s#%d: %s\n\nState: %s\nLabels: %v\n\n%s",
			ctx.Issue.Owner, ctx.Issue.Repo, ctx.Issue.Number, ctx.Issue.Title,
			ctx.Issue.State, ctx.Issue.Labels, ctx.Issue.Body)
		m.issueVP.SetContent(body)
	}
	if ctx.Snapshot != nil {
		m.scoutVP.SetContent(fmt.Sprintf(
			"Repo: %s\nFiles: %d\nTop languages: %v\n\nPress 'r' to run Scout.",
			ctx.Snapshot.Root, ctx.Snapshot.TotalFiles, ctx.Snapshot.TopLanguages(8),
		))
	}
	m.criticVP.SetContent("Critic has not run yet.\nPress 'r' after Scout to generate a recommendation.")
}

// applyEvent folds an orchestrator event into the TUI state. It rewrites the
// pane contents whenever the relevant artifact changes.
func (m *Model) applyEvent(ev orchestrator.Event) {
	if ev.Err != nil {
		m.lastErr = ev.Err
		m.status = "Error: " + ev.Err.Error()
		return
	}
	switch ev.State {
	case orchestrator.StateScouting:
		m.status = ev.Message
	case orchestrator.StateCritiquing:
		ctx := m.orch.AgentContext()
		if ctx.ScoutReport != "" {
			m.scoutVP.SetContent(ctx.ScoutReport)
		}
		m.status = ev.Message
	case orchestrator.StateAwaitUser:
		if rpt := m.orch.CriticReport(); rpt != nil {
			m.criticVP.SetContent(rpt.Markdown)
		}
		m.status = ev.Message + "  —  press 'a' to approve, 'k' to kill."
		m.pipelineDone = true
	case orchestrator.StateError:
		if ev.Err != nil {
			m.lastErr = ev.Err
			m.status = "Error: " + ev.Err.Error()
		}
	}
}
