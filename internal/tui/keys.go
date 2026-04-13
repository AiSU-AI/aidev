package tui

import "github.com/charmbracelet/bubbles/key"

// keyMap holds every keybinding the TUI responds to. Keeping them in one
// place makes the Help view and the Update switch statement agree by
// construction.
type keyMap struct {
	Quit       key.Binding
	Run        key.Binding
	Tab        key.Binding
	Approve    key.Binding
	Kill       key.Binding
	Help       key.Binding
	PickSketch key.Binding // 1-9 in phaseSketches kicks off the Implementer
	Test       key.Binding // 't' in phasePatchReady runs the Tester
	ReviewKey  key.Binding // 'v' in phasePatchReady/phaseTestsDone runs the Reviewer
}

func defaultKeys() keyMap {
	return keyMap{
		Quit:       key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
		Run:        key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "run pipeline")),
		Tab:        key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "cycle pane")),
		Approve:    key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "approve (build)")),
		Kill:       key.NewBinding(key.WithKeys("k"), key.WithHelp("k", "kill proposal")),
		Help:       key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
		PickSketch: key.NewBinding(key.WithKeys("1", "2", "3", "4", "5", "6", "7", "8", "9"), key.WithHelp("1-9", "pick sketch")),
		Test:       key.NewBinding(key.WithKeys("t"), key.WithHelp("t", "run tests")),
		ReviewKey:  key.NewBinding(key.WithKeys("v"), key.WithHelp("v", "review patch")),
	}
}
