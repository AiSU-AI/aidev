package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// Styling: we keep the palette small and let lipgloss handle the hard work
// of terminal colour profiles.
var (
	borderActive = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("205"))
	borderInactive = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("240"))
	title = lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("51")).
		Padding(0, 1)
	statusLine = lipgloss.NewStyle().
			Foreground(lipgloss.Color("244")).
			Padding(0, 1)
	errorLine = lipgloss.NewStyle().
			Foreground(lipgloss.Color("196")).
			Padding(0, 1)
)

// View renders the full TUI: three panes side-by-side (issue | scout | critic/sketches),
// a title at the top and a status line at the bottom. The rightmost pane
// retitles itself from "Critic report" to "Architect sketches" once the
// Architect has produced output.
func (m Model) View() string {
	if !m.ready {
		return "initialising aidev TUI..."
	}

	header := title.Render("aidev — Scout + Critic + Architect (v0.2a)")

	panel := func(name string, vp string, focused bool) string {
		style := borderInactive
		if focused {
			style = borderActive
		}
		return style.Render(lipgloss.JoinVertical(lipgloss.Left,
			title.Render(name),
			vp,
		))
	}

	issuePanel := panel("Issue", m.issueVP.View(), m.focus == paneIssue)
	scoutPanel := panel("Scout brief", m.scoutVP.View(), m.focus == paneScout)
	rightTitle := "Critic report"
	if m.showingSketches {
		rightTitle = "Architect sketches"
	}
	criticPanel := panel(rightTitle, m.criticVP.View(), m.focus == paneCritic)

	body := lipgloss.JoinHorizontal(lipgloss.Top, issuePanel, scoutPanel, criticPanel)

	var footer string
	if m.lastErr != nil {
		footer = errorLine.Render(m.status)
	} else {
		footer = statusLine.Render(m.status)
	}

	help := statusLine.Render(helpHint())

	return strings.Join([]string{
		header,
		body,
		footer,
		help,
	}, "\n")
}

func helpHint() string {
	return "tab: cycle panes   r: run   a: approve → Architect   k: kill   q: quit"
}
