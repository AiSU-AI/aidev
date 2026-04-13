package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/aisu-ai/aidev/internal/agents"
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
			m.outputVP = viewport.New(msg.Width-2*(msg.Width/3), msg.Height-6)
			m.ready = true
			m.hydrateInitialContent()
		} else {
			m.issueVP.Width = msg.Width / 3
			m.scoutVP.Width = msg.Width / 3
			m.outputVP.Width = msg.Width - 2*(msg.Width/3)
			m.issueVP.Height = msg.Height - 6
			m.scoutVP.Height = msg.Height - 6
			m.outputVP.Height = msg.Height - 6
		}

	case tea.KeyMsg:
		switch {
		case key.Matches(msg, m.keys.Quit):
			return m, tea.Quit
		case key.Matches(msg, m.keys.Tab):
			m.focus = (m.focus + 1) % numPanes
		case key.Matches(msg, m.keys.Run):
			if m.phase == phaseIdle {
				m.status = "Running Scout..."
				cmd = m.runPipelineCmd()
				return m, cmd
			}
		case key.Matches(msg, m.keys.Approve):
			if m.phase == phaseAwaitUser {
				m.status = "Starting Architect..."
				cmd = m.continuePipelineCmd()
				return m, cmd
			}
		case key.Matches(msg, m.keys.PickSketch):
			if m.phase == phaseSketches {
				// The key string is the digit character; convert.
				n := int(msg.String()[0] - '0')
				sketches := m.orch.AgentContext().Sketches
				if n >= 1 && n <= len(sketches) {
					m.status = fmt.Sprintf("Running Implementer on sketch %d...", n)
					cmd = m.implementCmd(n)
					return m, cmd
				}
				m.status = fmt.Sprintf("No sketch %d (have %d). Pick 1-%d.", n, len(sketches), len(sketches))
			}
		case key.Matches(msg, m.keys.Test):
			if m.phase == phasePatchReady || m.phase == phaseTestsDone {
				m.status = "Running tests..."
				cmd = m.testCmd()
				return m, cmd
			}
		case key.Matches(msg, m.keys.ReviewKey):
			if m.phase == phasePatchReady || m.phase == phaseTestsDone {
				if m.orch.Patch() == nil {
					m.status = "No patch to review. Run the Implementer first."
				} else {
					m.status = "Running Reviewer..."
					cmd = m.reviewCmd()
					return m, cmd
				}
			}
		case key.Matches(msg, m.keys.Kill):
			if m.phase == phaseAwaitUser || m.phase == phaseSketches {
				m.orch.Kill()
				m.phase = phaseKilled
				m.status = "Killed. No implementation will proceed."
				m.events = nil
			}
		}

	case eventMsg:
		m.applyEvent(msg.ev)
		if m.events != nil {
			cmd = waitForEvent(m.events)
		}
		return m, cmd

	case eventsClosedMsg:
		m.events = nil
	}

	// Forward to the focused viewport so the user can scroll.
	switch m.focus {
	case paneIssue:
		m.issueVP, cmd = m.issueVP.Update(msg)
	case paneScout:
		m.scoutVP, cmd = m.scoutVP.Update(msg)
	case paneOutput:
		m.outputVP, cmd = m.outputVP.Update(msg)
	}
	return m, cmd
}

// hydrateInitialContent fills the issue viewport with the already-loaded
// issue and leaves the scout/output panes empty until the pipeline runs.
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
	m.outputVP.SetContent("Critic has not run yet.\nPress 'r' after Scout to generate a recommendation.")
}

// applyEvent folds an orchestrator event into the TUI state. It rewrites the
// pane contents whenever the relevant artifact changes.
func (m *Model) applyEvent(ev orchestrator.Event) {
	if ev.Err != nil {
		m.lastErr = ev.Err
		m.status = "Error: " + ev.Err.Error()
		m.phase = phaseErrored
		return
	}
	switch ev.State {
	case orchestrator.StateScouting:
		m.phase = phaseRunning
		m.status = ev.Message
	case orchestrator.StateCritiquing:
		ctx := m.orch.AgentContext()
		if ctx.ScoutReport != "" {
			m.scoutVP.SetContent(ctx.ScoutReport)
		}
		m.phase = phaseRunning
		m.status = ev.Message
	case orchestrator.StateAwaitUser:
		if rpt := m.orch.CriticReport(); rpt != nil {
			m.outputVP.SetContent(rpt.Markdown)
		}
		m.outputTitle = "Critic report"
		m.phase = phaseAwaitUser
		preview := m.orch.CostPreview()
		m.status = fmt.Sprintf("%s  —  press 'a' to approve (Architect: %d sketches, ~%d tokens, ~%ds), 'k' to kill.",
			ev.Message, preview.N, preview.TotalOutputTokens, preview.EstimatedSeconds)
	case orchestrator.StateArchitecting:
		m.phase = phaseArchitect
		m.status = ev.Message
	case orchestrator.StateSketchesReady:
		m.outputTitle = "Architect sketches"
		m.phase = phaseSketches
		m.outputVP.SetContent(renderSketches(m.orch.AgentContext().Sketches))
		sketches := m.orch.AgentContext().Sketches
		m.status = fmt.Sprintf("%s  —  press 1-%d to pick a sketch and run the Implementer, 'k' to kill.",
			ev.Message, len(sketches))
	case orchestrator.StateImplementing:
		m.phase = phaseImplementing
		m.status = ev.Message
	case orchestrator.StatePatchReady:
		m.outputTitle = "Implementer patch"
		if patch := m.orch.Patch(); patch != nil {
			m.outputVP.SetContent(patch.Diff)
		}
		m.phase = phasePatchReady
		m.status = ev.Message + "  —  press 't' to run tests, 'v' to review the patch, 'q' to quit."
	case orchestrator.StateTesting:
		m.phase = phaseTesting
		m.status = ev.Message
	case orchestrator.StateTestsPassed, orchestrator.StateTestsFailed:
		m.outputTitle = "Test result"
		if r := m.orch.TestResult(); r != nil {
			m.outputVP.SetContent(renderTestResult(r))
		}
		m.phase = phaseTestsDone
		m.status = ev.Message + "  —  press 'v' to review, 't' to re-run tests, 'q' to quit."
	case orchestrator.StateReviewing:
		m.phase = phaseReviewing
		m.status = ev.Message
	case orchestrator.StateReviewDone:
		m.outputTitle = "Review"
		if rev := m.orch.Review(); rev != nil {
			m.outputVP.SetContent(rev.Markdown)
		}
		m.phase = phaseReviewDone
		m.status = ev.Message + "  —  press 'q' to quit."
	case orchestrator.StateError:
		if ev.Err != nil {
			m.lastErr = ev.Err
			m.status = "Error: " + ev.Err.Error()
		}
		m.phase = phaseErrored
	}
}

// renderSketches concatenates the Architect's sketches into one scrollable
// Markdown document for the pane to display. We keep it unopinionated about
// layout: the sketch headings are already "## Sketch N: Title" which
// renders as section breaks.
func renderSketches(sketches []agents.Sketch) string {
	if len(sketches) == 0 {
		return "Architect produced no sketches."
	}
	var b strings.Builder
	for i, s := range sketches {
		if i > 0 {
			b.WriteString("\n\n---\n\n")
		}
		b.WriteString(s.Markdown)
	}
	return b.String()
}

// renderTestResult formats a TestResult for the output pane — the headline
// pass/fail verdict, command, duration, output tail, and the LLM summary
// if the run failed.
func renderTestResult(r *agents.TestResult) string {
	var b strings.Builder
	verdict := "PASSED"
	if !r.Passed {
		verdict = "FAILED"
	}
	fmt.Fprintf(&b, "# %s\n\n", verdict)
	fmt.Fprintf(&b, "**command:** `%s`\n", r.Command)
	fmt.Fprintf(&b, "**detected:** %s\n", r.Detected)
	fmt.Fprintf(&b, "**exit code:** %d\n", r.ExitCode)
	fmt.Fprintf(&b, "**duration:** %s\n\n", r.Duration)
	if r.Summary != "" {
		b.WriteString("## Failure summary\n\n")
		b.WriteString(r.Summary)
		b.WriteString("\n\n")
	}
	if r.Output != "" {
		b.WriteString("## Output (tail)\n\n```\n")
		b.WriteString(r.Output)
		b.WriteString("\n```\n")
	}
	return b.String()
}

// formatIdleStatus produces the initial status line shown before the user
// presses 'r'. It exposes the cost preview up front so the user knows what
// the default Architect run would cost if the pipeline ends up running it.
func formatIdleStatus(n, tokens, seconds int) string {
	return fmt.Sprintf("Press 'r' to run Scout + Critic.  (If approved, Architect will produce %d sketches: ~%d tokens, ~%ds)",
		n, tokens, seconds)
}
