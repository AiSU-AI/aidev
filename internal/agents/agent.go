// Package agents hosts the individual agents aidev uses to turn an issue
// into a (critiqued, tested, documented) solution.
//
// v0.1 shipped Scout and Critic.
// v0.2a adds the Architect; Implementer, Tester, and Reviewer are still
// stubs in the orchestrator until later milestones.
//
// Agents share a tiny contract: Run takes the current Context and the agent's
// own Input, and returns a typed Output plus an error. Agents are
// deliberately NOT a plugin system — they are in-process, strongly typed,
// and their interfaces live in this package so callers see exactly which
// agent produced what.
package agents

import (
	"github.com/aisu-ai/aidev/internal/github"
	"github.com/aisu-ai/aidev/internal/repo"
)

// Context is the shared state an agent reads from. It is produced once per
// orchestrator run and mutated only at well-defined transitions — never by
// the agent itself.
//
// The report fields (ScoutReport, CriticReport) are deliberately plain
// strings rather than typed structs so downstream agents can quote them
// verbatim into prompts without unmarshalling. The Sketches slice is typed
// because the TUI needs to navigate it.
type Context struct {
	Issue        *github.Issue
	Snapshot     *repo.Snapshot
	Principles   []Principle
	ScoutReport  string   // filled in after Scout runs
	CriticReport string   // filled in after Critic runs
	Sketches     []Sketch // filled in after Architect runs

	// ClarifierNotes holds a compact markdown rendering of a Clarifier
	// session (the Critic's sharp questions plus the human's answers)
	// collected in the CURRENT orchestrator run. When non-empty it is
	// threaded into the Critic prompt so a second pass can reach a
	// verdict with the ambiguities resolved.
	//
	// This is distinct from Snapshot.ClarifierContent, which is the
	// durable `.aidev/clarifier.md` file on disk. ClarifierNotes is
	// authoritative for the in-memory re-critique loop; the on-disk
	// file is what downstream runs pick up.
	ClarifierNotes string
}

// Principle is re-exported so agent code doesn't need to import the config
// package directly. This mirrors the config.Principle shape intentionally.
type Principle struct {
	Name        string
	Summary     string
	Description string
}
