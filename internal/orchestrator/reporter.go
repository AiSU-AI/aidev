// Package orchestrator's Reporter is the single side-effect boundary
// for the pipeline. Agents stay pure; the orchestrator drives the state
// machine and fans every transition into both the TUI (via a channel) and
// the Reporter (via a method call). Implementations of Reporter decide
// whether those events become GitHub comments, Slack messages, log lines,
// or nothing at all.
package orchestrator

import "context"

// Reporter is the side-effect boundary. The orchestrator calls OnEvent
// synchronously for every state transition. Implementations that perform
// network I/O should time-box their work and never return errors that
// would abort the pipeline — audit trail is a nice-to-have, not a gate.
// Errors from OnEvent are logged by the orchestrator and pipeline
// execution continues.
type Reporter interface {
	// OnEvent is called synchronously from the orchestrator's run
	// goroutine after the event has been sent to the TUI channel. The
	// Event's Report field is a live pointer to the agents.Context at
	// emission time; implementations must read it synchronously because
	// the orchestrator may mutate the context on the next transition.
	OnEvent(ctx context.Context, ev Event) error

	// Close is called when the orchestrator is done with the reporter.
	// Implementations should flush any pending work here; errors are
	// logged but not propagated.
	Close() error
}

// NullReporter is a Reporter that does nothing. It is the default when
// the user passes -no-audit-trail, when the run has no GitHub issue to
// report against (e.g. the `aidev doctor` subcommand), or in tests.
type NullReporter struct{}

// OnEvent is a no-op.
func (NullReporter) OnEvent(context.Context, Event) error { return nil }

// Close is a no-op.
func (NullReporter) Close() error { return nil }
