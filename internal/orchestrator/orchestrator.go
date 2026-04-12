// Package orchestrator runs the aidev agent pipeline as an explicit state
// machine. v0.1 shipped three real states (scouting, critiquing, await_user)
// plus terminal error/killed states.
//
// v0.2a extends the machine with:
//
//   - StateArchitecting — the Architect is producing sketches
//   - StateSketchesReady — sketches are ready for the developer to review
//
// The machine is still linear: every run goes scout -> critic -> await_user,
// and a subsequent Continue() call transitions await_user -> architecting ->
// sketches_ready. Killed and errored runs terminate from any state.
//
// The orchestrator never talks to LLMs directly — it owns a Router and hands
// the right Provider to each agent via the agents package. This keeps
// agent-specific prompting out of the state machine.
package orchestrator

import (
	"context"
	"errors"
	"fmt"

	"github.com/aisu-ai/aidev/internal/agents"
	"github.com/aisu-ai/aidev/internal/config"
	"github.com/aisu-ai/aidev/internal/github"
	"github.com/aisu-ai/aidev/internal/llm"
	"github.com/aisu-ai/aidev/internal/repo"
)

// State is the orchestrator's finite state. Every transition moves from one
// State to the next or pushes an Event with an error.
type State string

const (
	StateInit           State = "init"
	StateScouting       State = "scouting"
	StateCritiquing     State = "critiquing"
	StateAwaitUser      State = "await_user"
	StateArchitecting   State = "architecting"
	StateSketchesReady  State = "sketches_ready"
	StateDone           State = "done"
	StateKilled         State = "killed"
	StateError          State = "error"
)

// Event names every externally observable thing the orchestrator does. The
// TUI subscribes to an Event channel and re-renders on each one.
type Event struct {
	State   State
	Message string
	Report  *agents.Context // snapshot of agent-visible state at the moment of emission
	Err     error
}

// Orchestrator owns the pipeline state for a single issue. Its zero value is
// NOT usable — construct via New().
type Orchestrator struct {
	cfg    *config.Config
	router *llm.Router
	gh     *github.Client

	state     State
	ctx       *agents.Context
	scout     *agents.Scout
	critic    *agents.Critic
	architect *agents.Architect
	critRpt   *agents.Report

	// sketchCount is the N the Architect uses when it runs. It is set by
	// New via the sketchCount config field and may be overridden at runtime
	// via SetSketchCount before Continue() is called.
	sketchCount int
}

// Option configures the orchestrator at construction time.
type Option func(*Orchestrator)

// WithSketchCount sets the number of sketches the Architect should produce.
// n <= 0 falls back to agents.DefaultSketchCount.
func WithSketchCount(n int) Option {
	return func(o *Orchestrator) {
		if n <= 0 {
			n = agents.DefaultSketchCount
		}
		o.sketchCount = n
	}
}

// New builds an Orchestrator from loaded config. It constructs the Router
// and every agent up front so that any misconfiguration surfaces before the
// TUI starts rendering.
func New(cfg *config.Config, opts ...Option) (*Orchestrator, error) {
	router, err := llm.NewRouter(cfg)
	if err != nil {
		return nil, err
	}
	o := &Orchestrator{
		cfg:         cfg,
		router:      router,
		gh:          github.NewClient(),
		state:       StateInit,
		ctx:         &agents.Context{},
		sketchCount: agents.DefaultSketchCount,
	}
	for _, opt := range opts {
		opt(o)
	}

	scout, err := agents.NewScout(router)
	if err != nil {
		return nil, err
	}
	critic, err := agents.NewCritic(router)
	if err != nil {
		return nil, err
	}
	architect, err := agents.NewArchitect(router, o.sketchCount)
	if err != nil {
		return nil, err
	}
	o.scout = scout
	o.critic = critic
	o.architect = architect
	return o, nil
}

// Router exposes the router so the TUI can display provider health.
func (o *Orchestrator) Router() *llm.Router { return o.router }

// State returns the current state.
func (o *Orchestrator) State() State { return o.state }

// AgentContext returns the current shared agent context. May contain zero
// values if the pipeline has not progressed through the relevant phase.
func (o *Orchestrator) AgentContext() *agents.Context { return o.ctx }

// CriticReport returns the last Critic report, if any.
func (o *Orchestrator) CriticReport() *agents.Report { return o.critRpt }

// SketchCount returns the N the Architect will use when Continue() is called.
func (o *Orchestrator) SketchCount() int { return o.sketchCount }

// CostPreview returns the Architect's pre-flight cost estimate for the
// currently configured sketch count. The TUI uses this to render "N sketches,
// ~X tokens, ~Y seconds" before the user approves.
func (o *Orchestrator) CostPreview() agents.CostEstimate {
	return agents.EstimateCost(o.sketchCount)
}

// LoadIssue pulls the issue identified by URL and seeds the agent context
// with it. Must be called before Run.
func (o *Orchestrator) LoadIssue(ctx context.Context, url string) error {
	owner, repoName, num, err := github.ParseURL(url)
	if err != nil {
		return err
	}
	issue, err := o.gh.Fetch(ctx, owner, repoName, num)
	if err != nil {
		return err
	}
	o.ctx.Issue = issue
	return nil
}

// LoadRepo scans a target directory and seeds the agent context with the
// snapshot plus any repo-local principles, merged on top of the global set.
func (o *Orchestrator) LoadRepo(root string) error {
	snap, err := repo.Scan(root)
	if err != nil {
		return err
	}
	o.ctx.Snapshot = snap

	// Start with the global principles, then merge in any repo-local ones.
	principles := make([]agents.Principle, 0, len(o.cfg.Principles.Principles))
	for _, p := range o.cfg.Principles.Principles {
		principles = append(principles, agents.Principle{
			Name: p.Name, Summary: p.Summary, Description: p.Description,
		})
	}
	if snap.PrinciplesPath != "" {
		extra, err := repo.LoadRepoPrinciples(snap.PrinciplesPath)
		if err != nil {
			return fmt.Errorf("load repo principles: %w", err)
		}
		have := make(map[string]bool, len(principles))
		for _, p := range principles {
			have[p.Name] = true
		}
		for _, p := range extra {
			if have[p.Name] {
				continue
			}
			principles = append(principles, agents.Principle{
				Name: p.Name, Summary: p.Summary, Description: p.Description,
			})
		}
	}
	o.ctx.Principles = principles
	return nil
}

// Run executes the first pass of the pipeline: scout -> critic -> await user.
// It emits one Event per state transition on the returned channel and closes
// the channel when the first pass terminates (at await_user, killed, or
// error). Call Continue() to drive the second pass (architect -> sketches).
func (o *Orchestrator) Run(ctx context.Context) <-chan Event {
	out := make(chan Event, 8)
	go func() {
		defer close(out)

		if o.ctx.Issue == nil {
			o.state = StateError
			out <- Event{State: o.state, Err: errors.New("orchestrator: no issue loaded")}
			return
		}
		if o.ctx.Snapshot == nil {
			o.state = StateError
			out <- Event{State: o.state, Err: errors.New("orchestrator: no repo loaded")}
			return
		}

		// Scout.
		o.state = StateScouting
		out <- Event{State: o.state, Message: "Scouting repository..."}
		if _, err := o.scout.Run(ctx, o.ctx); err != nil {
			o.state = StateError
			out <- Event{State: o.state, Err: err}
			return
		}

		// Critic.
		o.state = StateCritiquing
		out <- Event{State: o.state, Message: "Critic evaluating proposal..."}
		rpt, err := o.critic.Run(ctx, o.ctx)
		if err != nil {
			o.state = StateError
			out <- Event{State: o.state, Err: err}
			return
		}
		o.critRpt = rpt

		// First pass terminates here and awaits the human's verdict.
		o.state = StateAwaitUser
		out <- Event{State: o.state, Message: "Critic recommends: " + rpt.Recommendation}
	}()
	return out
}

// Continue runs the Architect stage. Must be called after Run() has
// transitioned to StateAwaitUser; the TUI typically calls it when the user
// presses the approve key. The returned channel emits architecting events
// and closes when sketches are ready (or the run errors).
//
// Calling Continue from any state other than StateAwaitUser returns a
// channel that immediately emits an error event and closes.
func (o *Orchestrator) Continue(ctx context.Context) <-chan Event {
	out := make(chan Event, 4)
	go func() {
		defer close(out)

		if o.state != StateAwaitUser {
			o.state = StateError
			out <- Event{
				State: o.state,
				Err: fmt.Errorf("orchestrator: Continue called from state %q, expected await_user", o.state),
			}
			return
		}

		o.state = StateArchitecting
		preview := agents.EstimateCost(o.sketchCount)
		out <- Event{
			State: o.state,
			Message: fmt.Sprintf("Architect generating %d sketches (~%d tokens, ~%ds)...",
				preview.N, preview.TotalOutputTokens, preview.EstimatedSeconds),
		}

		sketches, err := o.architect.Run(ctx, o.ctx)
		if err != nil {
			o.state = StateError
			out <- Event{State: o.state, Err: err}
			return
		}
		o.ctx.Sketches = sketches

		o.state = StateSketchesReady
		out <- Event{
			State: o.state,
			Message: fmt.Sprintf("Architect produced %d sketches. Review them and pick one.", len(sketches)),
		}
	}()
	return out
}

// Kill transitions the orchestrator to StateKilled. It is safe to call from
// any non-terminal state and idempotent once terminal.
func (o *Orchestrator) Kill() {
	if o.state == StateDone || o.state == StateKilled || o.state == StateError {
		return
	}
	o.state = StateKilled
}
