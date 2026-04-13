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
// v0.2b.1 adds the Reporter hook: every state transition is fanned out to
// both the TUI event channel (as before) and a Reporter that owns all
// outgoing side effects (GitHub comments, labels, etc.). Agents stay pure.
//
// The orchestrator never talks to LLMs directly — it owns a Router and hands
// the right Provider to each agent via the agents package. This keeps
// agent-specific prompting out of the state machine.
package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

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
	StateInit          State = "init"
	StateScouting      State = "scouting"
	StateCritiquing    State = "critiquing"
	StateAwaitUser     State = "await_user"
	StateArchitecting  State = "architecting"
	StateSketchesReady State = "sketches_ready"
	StateImplementing  State = "implementing"
	StatePatchReady    State = "patch_ready"
	StateTesting       State = "testing"
	StateTestsPassed   State = "tests_passed"
	StateTestsFailed   State = "tests_failed"
	StateReviewing     State = "reviewing"
	StateReviewDone    State = "review_done"
	StateDone          State = "done"
	StateKilled        State = "killed"
	StateError         State = "error"
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

	state       State
	ctx         *agents.Context
	scout       *agents.Scout
	critic      *agents.Critic
	architect   *agents.Architect
	implementer *agents.Implementer
	tester      *agents.Tester
	reviewer    *agents.Reviewer
	critRpt     *agents.Report
	patch       *agents.Patch
	testResult  *agents.TestResult
	review      *agents.Review

	// sketchCount is the N the Architect uses when it runs. It is set by
	// New via the sketchCount config field and may be overridden at runtime
	// via SetSketchCount before Continue() is called.
	sketchCount int

	// reporter is the side-effect boundary. NullReporter by default;
	// the main entry point swaps in a GitHubReporter unless the user
	// passes -no-audit-trail. Never nil after New() returns.
	reporter Reporter

	// reporterLog is where reporter errors are logged when the reporter
	// itself returns an error from OnEvent. Defaults to os.Stderr.
	reporterLog io.Writer
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

// WithReporter installs a custom Reporter. When omitted, the orchestrator
// uses a NullReporter (no side effects). Passing nil is treated the same
// as omitting the option.
func WithReporter(r Reporter) Option {
	return func(o *Orchestrator) {
		if r != nil {
			o.reporter = r
		}
	}
}

// WithReporterLog sets the io.Writer that reporter errors are logged to.
// Defaults to os.Stderr when unset.
func WithReporterLog(w io.Writer) Option {
	return func(o *Orchestrator) {
		if w != nil {
			o.reporterLog = w
		}
	}
}

// GitHubClient exposes the orchestrator's github client so callers (like
// main.go) can build a GitHubReporter against the same client without
// duplicating credential handling.
func (o *Orchestrator) GitHubClient() *github.Client { return o.gh }

// SetReporter installs a Reporter after construction. Most callers should
// use WithReporter(...) at New time, but main.go needs to build the
// GitHubReporter *after* the issue is loaded, so we expose a post-hoc
// setter as well. Passing nil resets to NullReporter.
func (o *Orchestrator) SetReporter(r Reporter) {
	if r == nil {
		o.reporter = NullReporter{}
		return
	}
	o.reporter = r
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
		reporter:    NullReporter{},
		reporterLog: os.Stderr,
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
	implementer, err := agents.NewImplementer(router)
	if err != nil {
		return nil, err
	}
	tester, err := agents.NewTester(router)
	if err != nil {
		return nil, err
	}
	reviewer, err := agents.NewReviewer(router)
	if err != nil {
		return nil, err
	}
	o.scout = scout
	o.critic = critic
	o.architect = architect
	o.implementer = implementer
	o.tester = tester
	o.reviewer = reviewer
	return o, nil
}

// Review returns the last Reviewer output, if any.
func (o *Orchestrator) Review() *agents.Review { return o.review }

// ReviewPatch runs the Reviewer against the given patch string and
// emits reviewing → review_done. Proposed follow-ups are written to
// `<repo>/.aidev/followups.md` if any are present. The patch argument
// lets callers supply either the orchestrator's own Implementer output
// or an external diff file (for `aidev review -patch foo.diff`).
func (o *Orchestrator) ReviewPatch(ctx context.Context, patch string) <-chan Event {
	out := make(chan Event, 4)
	go func() {
		defer close(out)
		if o.ctx.Issue == nil {
			o.state = StateError
			o.emit(ctx, out, Event{State: StateError, Err: errors.New("orchestrator: no issue loaded")})
			return
		}
		o.state = StateReviewing
		o.emit(ctx, out, Event{State: o.state, Message: "Reviewer performing Boy Scout pass..."})

		rev, err := o.reviewer.Run(ctx, o.ctx, patch)
		if err != nil {
			o.state = StateError
			o.emit(ctx, out, Event{State: StateError, Err: err})
			return
		}
		o.review = rev

		// Best-effort follow-up persistence.
		if o.ctx.Snapshot != nil && len(rev.FollowUps) > 0 {
			if _, werr := rev.WriteFollowUps(o.ctx.Snapshot.Root); werr != nil {
				fmt.Fprintf(o.reporterLog, "aidev reviewer: write followups: %v\n", werr)
			}
		}

		o.state = StateReviewDone
		o.emit(ctx, out, Event{
			State: o.state,
			Message: fmt.Sprintf("Review complete: %d blockers, %d suggestions, %d follow-ups, verdict %q",
				len(rev.Blockers), len(rev.Suggests), len(rev.FollowUps), rev.Verdict),
		})
	}()
	return out
}

// TestResult returns the last Tester result, if any.
func (o *Orchestrator) TestResult() *agents.TestResult { return o.testResult }

// Test runs the Tester against the current working tree of the loaded
// repository and emits tests_passed / tests_failed. It is valid from
// any state once the repo has been loaded — the Tester is a
// first-class side operation, not tightly coupled to the pipeline
// phase. This lets the user invoke it directly via `aidev test` or
// trigger it from inside the pipeline after patch_ready.
func (o *Orchestrator) Test(ctx context.Context) <-chan Event {
	out := make(chan Event, 4)
	go func() {
		defer close(out)
		if o.ctx.Snapshot == nil {
			o.state = StateError
			o.emit(ctx, out, Event{State: StateError, Err: errors.New("orchestrator: no repo loaded")})
			return
		}
		o.state = StateTesting
		o.emit(ctx, out, Event{State: o.state, Message: "Running test suite..."})

		result, err := o.tester.Run(ctx, o.ctx.Snapshot.Root)
		if err != nil {
			o.state = StateError
			o.emit(ctx, out, Event{State: StateError, Err: err})
			return
		}
		o.testResult = result

		if result.Passed {
			o.state = StateTestsPassed
		} else {
			o.state = StateTestsFailed
		}
		o.emit(ctx, out, Event{
			State: o.state,
			Message: fmt.Sprintf("Tests %s (%s, exit %d, %s)",
				passedOrFailed(result.Passed), result.Command, result.ExitCode, result.Duration.Round(time.Millisecond)),
		})
	}()
	return out
}

// passedOrFailed is a tiny helper for the test result message.
func passedOrFailed(b bool) string {
	if b {
		return "PASSED"
	}
	return "FAILED"
}

// Patch returns the last Implementer patch, if any.
func (o *Orchestrator) Patch() *agents.Patch { return o.patch }

// Implement runs the Implementer against the chosen sketch index
// (1-indexed into c.Sketches). Must be called after StateSketchesReady.
// Emits a patch_ready event on success and writes the diff to
// `<repo>/.aidev/proposed.patch` so the user can `git apply` it.
func (o *Orchestrator) Implement(ctx context.Context, sketchNumber int) <-chan Event {
	out := make(chan Event, 4)
	go func() {
		defer close(out)

		if o.state != StateSketchesReady {
			o.state = StateError
			o.emit(ctx, out, Event{
				State: StateError,
				Err:   fmt.Errorf("orchestrator: Implement called from state %q, expected sketches_ready", o.state),
			})
			return
		}
		if sketchNumber < 1 || sketchNumber > len(o.ctx.Sketches) {
			o.state = StateError
			o.emit(ctx, out, Event{
				State: StateError,
				Err:   fmt.Errorf("orchestrator: sketch %d out of range (have %d)", sketchNumber, len(o.ctx.Sketches)),
			})
			return
		}

		chosen := o.ctx.Sketches[sketchNumber-1]

		o.state = StateImplementing
		o.emit(ctx, out, Event{
			State:   o.state,
			Message: fmt.Sprintf("Implementing sketch %d: %s", chosen.Number, chosen.Title),
		})

		patch, err := o.implementer.Run(ctx, o.ctx, &chosen)
		if err != nil {
			o.state = StateError
			o.emit(ctx, out, Event{State: StateError, Err: err})
			return
		}

		// Write the patch to disk so the user can git apply it. Failure
		// here is non-fatal — we still return the patch in memory.
		if o.ctx.Snapshot != nil {
			if _, werr := patch.WriteTo(o.ctx.Snapshot.Root); werr != nil {
				fmt.Fprintf(o.reporterLog, "aidev implementer: write patch: %v\n", werr)
			}
		}
		o.patch = patch

		o.state = StatePatchReady
		msg := fmt.Sprintf("Patch ready: %d files touched.", len(patch.FilesTouched))
		if patch.Path != "" {
			msg += " Saved to " + patch.Path
		}
		o.emit(ctx, out, Event{State: o.state, Message: msg})
	}()
	return out
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
// error). Every emitted event is also fanned out to the Reporter. Call
// Continue() to drive the second pass (architect -> sketches).
func (o *Orchestrator) Run(ctx context.Context) <-chan Event {
	out := make(chan Event, 8)
	go func() {
		defer close(out)

		if o.ctx.Issue == nil {
			o.emit(ctx, out, Event{State: StateError, Err: errors.New("orchestrator: no issue loaded")})
			return
		}
		if o.ctx.Snapshot == nil {
			o.emit(ctx, out, Event{State: StateError, Err: errors.New("orchestrator: no repo loaded")})
			return
		}

		// Scout.
		o.state = StateScouting
		o.emit(ctx, out, Event{State: o.state, Message: "Scouting repository..."})
		if _, err := o.scout.Run(ctx, o.ctx); err != nil {
			o.state = StateError
			o.emit(ctx, out, Event{State: o.state, Err: err})
			return
		}

		// Critic.
		o.state = StateCritiquing
		o.emit(ctx, out, Event{State: o.state, Message: "Critic evaluating proposal..."})
		rpt, err := o.critic.Run(ctx, o.ctx)
		if err != nil {
			o.state = StateError
			o.emit(ctx, out, Event{State: o.state, Err: err})
			return
		}
		o.critRpt = rpt

		// First pass terminates here and awaits the human's verdict.
		o.state = StateAwaitUser
		o.emit(ctx, out, Event{State: o.state, Message: "Critic recommends: " + rpt.Recommendation})
	}()
	return out
}

// emit fans an event out to both the TUI channel and the Reporter. It
// stamps the event with a pointer to the current agents.Context so the
// Reporter can read downstream artifacts (ScoutReport, CriticReport,
// Sketches) from the one place that holds them. Reporter errors are
// logged to o.reporterLog and never propagated — the audit trail is a
// best-effort facility, not a gate.
func (o *Orchestrator) emit(ctx context.Context, out chan<- Event, ev Event) {
	ev.Report = o.ctx
	out <- ev
	if o.reporter == nil {
		return
	}
	if err := o.reporter.OnEvent(ctx, ev); err != nil {
		fmt.Fprintf(o.reporterLog, "aidev reporter: %v\n", err)
	}
}

// Continue runs the Architect stage. Must be called after Run() has
// transitioned to StateAwaitUser; the TUI typically calls it when the user
// presses the approve key. The returned channel emits architecting events
// and closes when sketches are ready (or the run errors). Events are also
// fanned to the Reporter.
//
// Calling Continue from any state other than StateAwaitUser returns a
// channel that immediately emits an error event and closes.
func (o *Orchestrator) Continue(ctx context.Context) <-chan Event {
	out := make(chan Event, 4)
	go func() {
		defer close(out)

		if o.state != StateAwaitUser {
			o.emit(ctx, out, Event{
				State: StateError,
				Err:   fmt.Errorf("orchestrator: Continue called from state %q, expected await_user", o.state),
			})
			o.state = StateError
			return
		}

		o.state = StateArchitecting
		preview := agents.EstimateCost(o.sketchCount)
		o.emit(ctx, out, Event{
			State: o.state,
			Message: fmt.Sprintf("Architect generating %d sketches (~%d tokens, ~%ds)...",
				preview.N, preview.TotalOutputTokens, preview.EstimatedSeconds),
		})

		sketches, err := o.architect.Run(ctx, o.ctx)
		if err != nil {
			o.state = StateError
			o.emit(ctx, out, Event{State: o.state, Err: err})
			return
		}
		o.ctx.Sketches = sketches

		o.state = StateSketchesReady
		o.emit(ctx, out, Event{
			State:   o.state,
			Message: fmt.Sprintf("Architect produced %d sketches. Review them and pick one.", len(sketches)),
		})
	}()
	return out
}

// Kill transitions the orchestrator to StateKilled and fires a terminal
// event to both the TUI channel and the Reporter. It is safe to call from
// any non-terminal state and idempotent once terminal.
func (o *Orchestrator) Kill() {
	if o.state == StateDone || o.state == StateKilled || o.state == StateError {
		return
	}
	o.state = StateKilled
	// Fire a synthetic terminal event so the Reporter can finalise its
	// audit trail. We use a one-shot channel the caller drops.
	ctx := context.Background()
	if o.reporter != nil {
		ev := Event{State: StateKilled, Message: "Killed by user", Report: o.ctx}
		if err := o.reporter.OnEvent(ctx, ev); err != nil {
			fmt.Fprintf(o.reporterLog, "aidev reporter: %v\n", err)
		}
	}
}

// CloseReporter releases the reporter. Safe to call after the
// orchestrator is done; main.go uses this from a defer.
func (o *Orchestrator) CloseReporter() error {
	if o.reporter == nil {
		return nil
	}
	return o.reporter.Close()
}
