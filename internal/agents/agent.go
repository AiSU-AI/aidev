// Package agents hosts the individual agents aidev uses to turn an issue
// into a (critiqued, tested, documented) solution.
//
// v0.1 ships only the Scout and Critic; the Architect, Implementer, Tester,
// and Reviewer are stubs in the orchestrator until the next milestone.
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
type Context struct {
	Issue       *github.Issue
	Snapshot    *repo.Snapshot
	Principles  []Principle
	ScoutReport string // filled in after Scout runs
}

// Principle is re-exported so agent code doesn't need to import the config
// package directly. This mirrors the config.Principle shape intentionally.
type Principle struct {
	Name        string
	Summary     string
	Description string
}
