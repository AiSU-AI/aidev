// Package orchestrator runs the aidev agent pipeline as an explicit state
// machine. v0.1 only models three real states (scouting, critiquing, done)
// plus the terminal error/killed states; later milestones add architect,
// implementer, tester, and reviewer transitions.
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

// State is the orchestrator's finite state. Every Step() call moves from
// one State to the next or returns an error.
type State string

const (
	StateInit       State = "init"
	StateScouting   State = "scouting"
	StateCritiquing State = "critiquing"
	StateAwaitUser  State = "await_user"
	StateDone       State = "done"
	StateKilled     State = "killed"
	StateError      State = "error"
)

// Event names every externally observable thing the orchestrator does. The
// TUI subscribes to an Event channel and re-renders on each one.
type Event struct {
	State   State
	Message string
	Report  *agents.Context // snapshot of agent-visible state at the moment of emission
	Err     error
}

// Orchestrator owns the pipeline state for a single issue.
type Orchestrator struct {
	cfg    *config.Config
	router *llm.Router
	gh     *github.Client

	state   State
	ctx     *agents.Context
	critic  *agents.Critic
	scout   *agents.Scout
	critRpt *agents.Report
}

// New builds an Orchestrator from loaded config. It constructs the Router
// and the Scout/Critic agents up front so that any misconfiguration surfaces
// before the TUI starts rendering.
func New(cfg *config.Config) (*Orchestrator, error) {
	router, err := llm.NewRouter(cfg)
	if err != nil {
		return nil, err
	}
	scout, err := agents.NewScout(router)
	if err != nil {
		return nil, err
	}
	critic, err := agents.NewCritic(router)
	if err != nil {
		return nil, err
	}
	return &Orchestrator{
		cfg:    cfg,
		router: router,
		gh:     github.NewClient(),
		state:  StateInit,
		scout:  scout,
		critic: critic,
		ctx:    &agents.Context{},
	}, nil
}

// Router exposes the router so the TUI can display provider health.
func (o *Orchestrator) Router() *llm.Router { return o.router }

// State returns the current state.
func (o *Orchestrator) State() State { return o.state }

// AgentContext returns the current shared agent context (may contain zero
// values if pipeline has not progressed).
func (o *Orchestrator) AgentContext() *agents.Context { return o.ctx }

// CriticReport returns the last Critic report, if any.
func (o *Orchestrator) CriticReport() *agents.Report { return o.critRpt }

// LoadIssue pulls the issue identified by URL and seeds the agent context
// with it. Must be called before Scout.
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

// Run executes the v0.1 pipeline: scout -> critic -> await user. It emits
// one Event per state transition on the returned channel and closes the
// channel when the pipeline terminates (either at await_user, done, killed,
// or error).
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

		// v0.1 terminates here and awaits the human's verdict.
		o.state = StateAwaitUser
		out <- Event{State: o.state, Message: "Critic recommends: " + rpt.Recommendation}
	}()
	return out
}
