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
	"path/filepath"
	"time"

	"github.com/aisu-ai/aidev/internal/agents"
	"github.com/aisu-ai/aidev/internal/config"
	"github.com/aisu-ai/aidev/internal/github"
	"github.com/aisu-ai/aidev/internal/llm"
	"github.com/aisu-ai/aidev/internal/repo"
	"github.com/aisu-ai/aidev/internal/runpath"
)

// State is the orchestrator's finite state. Every transition moves from one
// State to the next or pushes an Event with an error.
type State string

const (
	StateInit          State = "init"
	StateScouting      State = "scouting"
	StateCritiquing    State = "critiquing"
	StateAwaitUser     State = "await_user"
	StateArchitecting    State = "architecting"
	StateSelecting       State = "selecting"
	StateSketchesReady   State = "sketches_ready"
	// StateNeedsRefinement is the terminal state when aidev refuses
	// to proceed autonomously: Critic's last verdict is unclear/defer
	// after the Clarifier loop maxed out, the Architect produced no
	// viable sketches, OR the Selector found no implementable sketch.
	// The slash command (/aidev-run) detects this state and stops
	// without filing a PR; the reporter posts a "needs refinement"
	// comment with actionable next steps for the human.
	StateNeedsRefinement State = "needs_refinement"
	StateImplementing  State = "implementing"
	StatePatchReady    State = "patch_ready"
	StateTesting       State = "testing"
	StateTestsPassed   State = "tests_passed"
	StateTestsFailed   State = "tests_failed"
	StateReviewing     State = "reviewing"
	StateReviewDone    State = "review_done"
	StateTriaging      State = "triaging"
	StateTriageDone    State = "triage_done"
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
	selector    *agents.Selector
	implementer *agents.Implementer
	coordinator *agents.Coordinator
	tester      *agents.Tester
	reviewer    *agents.Reviewer
	triage      *agents.Triage
	critRpt     *agents.Report
	patch       *agents.Patch
	coordReview *agents.CoordinatorReview
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

	// runDir is the per-issue artifact directory aidev writes generated
	// outputs into (clarifier.md, architect-output.md, followups.md,
	// proposed.patch). Computed in LoadIssue from the issue identifier.
	// Empty string means LoadIssue has not run yet — callers that need
	// runDir should call RunDir() which surfaces a clear error in that
	// case rather than silently writing to a wrong path.
	runDir string
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
	// Selector — autonomous sketch picker (P0). Construction is
	// best-effort: if the rubric or router routing fails, we log and
	// continue without a Selector. The orchestrator's Continue()
	// handles a nil selector by falling through to manual pick.
	rubric, rerr := agents.LoadDefaultRubric()
	var selector *agents.Selector
	if rerr != nil {
		fmt.Fprintf(os.Stderr, "aidev: load default rubric: %v (selector disabled)\n", rerr)
	} else {
		selector, err = agents.NewSelector(router, rubric)
		if err != nil {
			fmt.Fprintf(os.Stderr, "aidev: build selector: %v (autonomous sketch pick disabled)\n", err)
			selector = nil
		}
	}
	implementer, err := agents.NewImplementer(router)
	if err != nil {
		return nil, err
	}
	coordinator, err := agents.NewCoordinator(router)
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
	// Triage — autonomous review-loop meta-judge (P6). Same best-effort
	// construction pattern as the Selector: when the routing fails we
	// log and continue without a Triage agent. Callers detect the
	// missing agent via Triage() returning nil and fall back to manual
	// review handling.
	triage, terr := agents.NewTriage(router)
	if terr != nil {
		fmt.Fprintf(os.Stderr, "aidev: build triage: %v (autonomous review loop disabled)\n", terr)
		triage = nil
	}
	o.scout = scout
	o.critic = critic
	o.architect = architect
	o.selector = selector
	o.implementer = implementer
	o.coordinator = coordinator
	o.tester = tester
	o.reviewer = reviewer
	o.triage = triage
	return o, nil
}

// Review returns the last Reviewer output, if any.
func (o *Orchestrator) Review() *agents.Review { return o.review }

// Triage runs the meta-judgment step (P6) on the most recent Reviewer
// output. Caller supplies the patch under review, optional CI status,
// the round number (1-indexed), prior-round actions for cycle
// protection, and any per-repo protected-path overrides. Returns the
// structured verdict the slash command consumes; nil if Triage is
// unavailable (no router routing for RoleTriage and no Critic
// fallback) or if Review() has not been called.
func (o *Orchestrator) Triage(ctx context.Context, patch string, ci *agents.CIStatus, round int, prev []agents.TriageAction, protectedPaths []string) (*agents.TriageVerdict, error) {
	if o.triage == nil {
		return nil, errors.New("orchestrator: triage agent not constructed")
	}
	if o.review == nil {
		return nil, errors.New("orchestrator: no review available — call ReviewPatch first")
	}
	o.state = StateTriaging
	v, err := o.triage.Run(llm.WithGate(ctx, "triage"), o.ctx, agents.TriageInput{
		Review:         o.review,
		Patch:          patch,
		CI:             ci,
		Round:          round,
		PrevActions:    prev,
		ProtectedPaths: protectedPaths,
	})
	if err != nil {
		o.state = StateError
		return nil, err
	}
	o.state = StateTriageDone
	return v, nil
}

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

		rev, err := o.reviewer.Run(llm.WithGate(ctx, "reviewer"), o.ctx, patch)
		if err != nil {
			o.state = StateError
			o.emit(ctx, out, Event{State: StateError, Err: err})
			return
		}
		o.review = rev

		// Best-effort follow-up persistence. Prefer runDir (P1
		// relocation); fall back to <repo>/.aidev/ when runDir is
		// empty (LoadIssue not run, or runpath resolution failed).
		if len(rev.FollowUps) > 0 {
			dir := o.runDir
			if dir == "" && o.ctx.Snapshot != nil {
				dir = filepath.Join(o.ctx.Snapshot.Root, ".aidev")
			}
			if dir != "" {
				if _, werr := rev.WriteFollowUps(dir); werr != nil {
					fmt.Fprintf(o.reporterLog, "aidev reviewer: write followups: %v\n", werr)
				}
			}
		}

		// Coordinator Gate 5: post-Reviewer monitor.
		o.runAdvisoryGate(ctx, out, "post-reviewer", func(ctx context.Context) ([]agents.GateNote, error) {
			return o.coordinator.ObserveReviewerVerdict(ctx, o.ctx, rev)
		})

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

// maxCoordinatorRounds is the cap on how many times the Implement
// loop will send the Implementer back with Coordinator feedback
// before giving up and writing the patch anyway (advisory mode).
// Two extra rounds past the initial attempt means up to 3
// Implementer runs per Implement() call in the worst case, which is
// bounded cost and enough headroom for the model to respond to
// specific concerns. If the Coordinator is still rejecting after
// three rounds, something is wrong that further retries won't fix.
const maxCoordinatorRounds = 2

// Implement runs the Implementer against the chosen sketch index
// (1-indexed into c.Sketches). Must be called after StateSketchesReady.
//
// The Implement flow is a bounded Implementer <-> Coordinator loop:
//
//  1. Implementer produces a diff.
//  2. Coordinator reviews the diff against the rubric (fabrication,
//     invalid syntax for the file format, auxiliary TODO files,
//     partial scope, dangling references, missing tests, scope
//     creep).
//  3. If APPROVED, write the patch to disk and emit patch_ready.
//  4. If CONCERNS and we're under the retry cap, stash the feedback
//     on ctx.CoordinatorFeedback and call the Implementer again.
//  5. If CONCERNS and we've hit the cap, write the patch anyway AND
//     surface the Coordinator's concerns in the patch_ready event's
//     advisory field. The user sees both the patch and the concerns
//     and decides. This is the deliberate "teammate, not gatekeeper"
//     default — we never silently drop work on the floor.
//
// Emits a patch_ready event on success (or on cap-reached advisory
// mode) and writes the diff to `<repo>/.aidev/proposed.patch` so the
// user can `git apply` it.
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

		// Clear any stale Coordinator feedback from a previous run
		// so it can't leak into the first attempt of this sketch.
		o.ctx.CoordinatorFeedback = ""
		o.coordReview = nil

		var patch *agents.Patch
		var review *agents.CoordinatorReview

		for round := 0; round <= maxCoordinatorRounds; round++ {
			p, err := o.implementer.Run(llm.WithGate(ctx, "implementer"), o.ctx, &chosen)
			if err != nil {
				o.state = StateError
				o.emit(ctx, out, Event{State: StateError, Err: err})
				return
			}
			patch = p

			// Gate 1: Coordinator review. A Coordinator error is
			// non-fatal — we log it and proceed as if approved.
			// The Coordinator is a safety net, not a gate; a broken
			// cloud connection shouldn't block the user's patch.
			r, cerr := o.coordinator.Review(llm.WithGate(ctx, "coordinator-review"), o.ctx, &chosen, patch)
			if cerr != nil {
				fmt.Fprintf(o.reporterLog, "aidev coordinator: review failed, proceeding without: %v\n", cerr)
				review = nil
				break
			}
			review = r

			if review.Approved {
				break
			}

			// CONCERNS. If we still have retry budget, send the
			// Implementer back with the feedback.
			if round == maxCoordinatorRounds {
				// Cap reached. Fall through with the latest patch
				// and the latest review so the user sees both.
				break
			}
			o.emit(ctx, out, Event{
				State:   o.state,
				Message: fmt.Sprintf("Coordinator flagged concerns (round %d/%d); re-running Implementer with feedback", round+1, maxCoordinatorRounds+1),
			})
			o.ctx.CoordinatorFeedback = review.Feedback
		}

		// Write the patch to disk so the user can git apply it. Failure
		// here is non-fatal — we still return the patch in memory.
		if o.ctx.Snapshot != nil {
			if _, werr := patch.WriteTo(o.ctx.Snapshot.Root); werr != nil {
				fmt.Fprintf(o.reporterLog, "aidev implementer: write patch: %v\n", werr)
			}
		}
		o.patch = patch
		o.coordReview = review

		o.state = StatePatchReady
		msg := fmt.Sprintf("Patch ready: %d files touched.", len(patch.FilesTouched))
		if patch.Path != "" {
			msg += " Saved to " + patch.Path
		}
		if review != nil && !review.Approved {
			msg += "\n\n⚠️ Coordinator still has concerns after " + fmt.Sprint(maxCoordinatorRounds+1) + " attempts — see Coordinator review below. Patch is advisory; review carefully before applying."
		} else if review != nil && review.Approved {
			msg += "\nCoordinator: APPROVED — " + review.Rationale
		}
		o.emit(ctx, out, Event{State: o.state, Message: msg})
	}()
	return out
}

// CoordinatorReview returns the last Coordinator review, if any.
// Nil when no Implement() call has run yet or when the Coordinator
// was unavailable on the most recent attempt (errors are logged, not
// propagated; the patch is still produced and this method returns
// nil in that case).
func (o *Orchestrator) CoordinatorReview() *agents.CoordinatorReview { return o.coordReview }

// CoordinatorNotes returns the running list of advisory notes the
// Coordinator's monitor gates have produced across the current
// run. Used by the headless report to surface what the
// post-Scout, post-Critic, post-Architect, and post-Reviewer
// gates observed. Empty when no gate has fired yet, or when the
// Coordinator wasn't reachable.
func (o *Orchestrator) CoordinatorNotes() []agents.GateNote {
	if o.coordinator == nil {
		return nil
	}
	return o.coordinator.Notes
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
// with it. Must be called before Run. Also computes the per-issue runDir
// (artifact directory under $XDG_DATA_HOME/aidev/runs/) so downstream
// gates can write generated artifacts there instead of polluting the
// target repo's working tree.
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

	// Compute runDir now so it's available to every subsequent gate.
	// Failure here is non-fatal — without runDir, callers fall back to
	// writing in the repo (legacy behaviour). Log the error so the
	// degradation is visible.
	dir, derr := runpath.RunDir(issue.Owner, issue.Repo, issue.Number)
	if derr != nil {
		fmt.Fprintf(o.reporterLog, "aidev: runpath: %v (artifacts will fall back to <repo>/.aidev/)\n", derr)
	} else {
		o.runDir = dir
	}
	return nil
}

// RunDir returns the per-issue artifact directory computed by LoadIssue.
// Empty string means LoadIssue has not run yet OR runpath resolution
// failed (in which case the legacy <repo>/.aidev/ path is used as a
// fallback). Callers that strictly need runDir should check for empty
// and decide what to do.
func (o *Orchestrator) RunDir() string { return o.runDir }

// Reporter returns the active Reporter. Callers (main.go's interview
// loop and force-verdict path) use this to post one-shot audit-trail
// comments that aren't part of the state-machine event stream.
// Returns nil if SetReporter explicitly cleared it; callers must
// nil-check (or type-assert via the helper below).
func (o *Orchestrator) Reporter() Reporter { return o.reporter }

// LoadRepo scans a target directory and seeds the agent context with the
// snapshot plus any repo-local principles, merged on top of the global set.
//
// If LoadIssue ran first AND a clarifier file exists in the per-issue
// runDir (`<runDir>/clarifier.md`), that clarifier overrides the
// in-repo `<repo>/.aidev/clarifier.md` that repo.Scan would otherwise
// surface. This is the migration path for P1 artifact relocation:
// new clarifier sessions land in runDir, old ones (left in <repo>/.aidev/)
// still get honoured as a fallback when no runDir version exists.
func (o *Orchestrator) LoadRepo(root string) error {
	snap, err := repo.Scan(root)
	if err != nil {
		return err
	}
	o.ctx.Snapshot = snap

	if o.runDir != "" {
		runClarifier := filepath.Join(o.runDir, "clarifier.md")
		if data, err := os.ReadFile(runClarifier); err == nil {
			snap.ClarifierPath = runClarifier
			snap.ClarifierContent = string(data)
		}
	}

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
		if _, err := o.scout.Run(llm.WithGate(ctx, "scout"), o.ctx); err != nil {
			o.state = StateError
			o.emit(ctx, out, Event{State: o.state, Err: err})
			return
		}

		// Coordinator Gate 0: post-Scout monitor.
		o.runAdvisoryGate(ctx, out, "post-scout", func(ctx context.Context) ([]agents.GateNote, error) {
			return o.coordinator.ObserveScoutBrief(ctx, o.ctx)
		})

		// Critic.
		o.state = StateCritiquing
		o.emit(ctx, out, Event{State: o.state, Message: "Critic evaluating proposal..."})
		rpt, err := o.critic.Run(llm.WithGate(ctx, "critic"), o.ctx)
		if err != nil {
			o.state = StateError
			o.emit(ctx, out, Event{State: o.state, Err: err})
			return
		}
		o.critRpt = rpt

		// Coordinator Gate 1: post-Critic monitor.
		o.runAdvisoryGate(ctx, out, "post-critic", func(ctx context.Context) ([]agents.GateNote, error) {
			return o.coordinator.ObserveCriticReport(ctx, o.ctx)
		})

		// First pass terminates here and awaits the human's verdict.
		o.state = StateAwaitUser
		o.emit(ctx, out, Event{State: o.state, Message: "Critic recommends: " + rpt.Recommendation})
	}()
	return out
}

// runAdvisoryGate is the shared wrapper for Gates 0/1/2/5. It
// invokes the named Coordinator method, logs any error to the
// reporter log (advisory gates NEVER block the pipeline), and
// emits a transition event so the headless report and the TUI
// can show what the monitor observed.
//
// Errors from the gate (provider failures, parse failures) are
// logged and swallowed. The pipeline must always be able to
// proceed past an advisory gate even if the Coordinator's
// provider is down — this is the safety-net contract from #33.
func (o *Orchestrator) runAdvisoryGate(ctx context.Context, out chan<- Event, gateName string, run func(context.Context) ([]agents.GateNote, error)) {
	if o.coordinator == nil {
		return
	}
	// Tag the gate name on the context so the telemetry middleware can
	// attribute Coordinator calls to the right advisory gate.
	gateCtx := llm.WithGate(ctx, "coordinator-"+gateName)
	notes, err := run(gateCtx)
	if err != nil {
		fmt.Fprintf(o.reporterLog, "aidev coordinator %s: %v\n", gateName, err)
		return
	}
	for _, n := range notes {
		// Surface concerns and warnings as events so the
		// headless report and TUI status line both show
		// them. Info-level breadcrumbs stay in the running
		// notes buffer only.
		switch n.Severity {
		case "warn", "concern":
			o.emit(ctx, out, Event{
				State:   o.state,
				Message: fmt.Sprintf("Coordinator [%s/%s]: %s", n.Gate, n.Severity, n.Body),
			})
			// Promote warn/concern severity to a standalone GH
			// comment so the audit trail captures the Coordinator's
			// observation alongside the Critic/Architect/Selector
			// decisions. The pinned status only shows the latest
			// transition; this gives every warning durable presence
			// on the issue thread. Failures are non-fatal — the
			// pipeline continued past the gate, the reporter post
			// is best-effort.
			if gr, ok := o.reporter.(*GitHubReporter); ok {
				if perr := gr.PostCoordinatorNote(ctx, gateName, n.Severity, n.Body); perr != nil {
					fmt.Fprintf(o.reporterLog, "aidev reporter: post coordinator note: %v\n", perr)
				}
			}
		}
	}
}

// Recritique re-runs ONLY the Critic stage against the current agent
// context. The expected caller is the headless interview loop: it has
// just collected Clarifier answers, stashed them on
// ctx.ClarifierNotes, and now wants a fresh verdict informed by those
// answers — without re-running Scout (the repo hasn't changed).
//
// Recritique is only valid when the orchestrator is sitting at
// StateAwaitUser. It overwrites the stored critic report and leaves
// the state at StateAwaitUser so the caller can decide what to do
// next (proceed to Continue, or interview again, or bail). Emits one
// event on the returned channel and closes.
func (o *Orchestrator) Recritique(ctx context.Context) <-chan Event {
	out := make(chan Event, 4)
	go func() {
		defer close(out)

		if o.state != StateAwaitUser {
			o.emit(ctx, out, Event{
				State: StateError,
				Err:   fmt.Errorf("orchestrator: Recritique called from state %q, expected await_user", o.state),
			})
			o.state = StateError
			return
		}
		if o.ctx == nil || o.ctx.ScoutReport == "" {
			o.emit(ctx, out, Event{
				State: StateError,
				Err:   errors.New("orchestrator: Recritique requires a prior Scout+Critic pass"),
			})
			o.state = StateError
			return
		}

		o.state = StateCritiquing
		o.emit(ctx, out, Event{State: o.state, Message: "Critic re-evaluating with clarifier answers..."})
		rpt, err := o.critic.Run(llm.WithGate(ctx, "critic-recritique"), o.ctx)
		if err != nil {
			o.state = StateError
			o.emit(ctx, out, Event{State: o.state, Err: err})
			return
		}
		o.critRpt = rpt

		o.state = StateAwaitUser
		o.emit(ctx, out, Event{State: o.state, Message: "Critic recommends: " + rpt.Recommendation})
	}()
	return out
}

// MarkNeedsRefinement transitions the orchestrator into
// StateNeedsRefinement with the supplied reason. It exists so the
// headless auto-run path in `cmd/aidev` can emit the structured
// "🤔 needs refinement" audit comment when the Critic returns unclear
// in an environment where the Clarifier interview couldn't resolve it
// (no TTY, or the interview ran and the verdict stayed unclear).
//
// This is the missing transition that issue #46 tracks: today, that
// codepath returns silently and the reporter applies the vague
// `aidev:awaiting-decision` label instead of the structured refinement
// comment + `aidev:needs-refinement` label the Selector-refusal path
// already produces. Reusing the existing StateNeedsRefinement
// emission keeps the contract (label, comment template, stdout marker
// via the caller) in one place.
//
// Valid from StateAwaitUser (the natural resting place after the
// first Run() pass when the Critic verdict didn't auto-advance the
// pipeline). Any other state is a misuse — we surface a StateError
// rather than corrupt the audit trail.
func (o *Orchestrator) MarkNeedsRefinement(ctx context.Context, reason string) <-chan Event {
	out := make(chan Event, 2)
	go func() {
		defer close(out)
		if o.state != StateAwaitUser {
			o.emit(ctx, out, Event{
				State: StateError,
				Err:   fmt.Errorf("orchestrator: MarkNeedsRefinement called from state %q, expected await_user", o.state),
			})
			o.state = StateError
			return
		}
		o.state = StateNeedsRefinement
		o.emit(ctx, out, Event{
			State:   o.state,
			Message: reason,
		})
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

		sketches, err := o.architect.Run(llm.WithGate(ctx, "architect"), o.ctx)
		if err != nil {
			o.state = StateError
			o.emit(ctx, out, Event{State: o.state, Err: err})
			return
		}
		o.ctx.Sketches = sketches

		// Coordinator Gate 2: post-Architect monitor.
		o.runAdvisoryGate(ctx, out, "post-architect", func(ctx context.Context) ([]agents.GateNote, error) {
			return o.coordinator.ObserveArchitectSketches(ctx, o.ctx)
		})

		// Selector gate (P0): autonomous sketch pick. The Selector
		// is optional — when nil (e.g. older orchestrator
		// constructions, tests that don't wire it), the pipeline
		// stops at sketches_ready and a human picks. When present,
		// it scores and picks before sketches_ready, leaving the
		// chosen sketch in o.ctx.Selector.ChosenNumber.
		if o.selector != nil {
			o.state = StateSelecting
			o.emit(ctx, out, Event{
				State:   o.state,
				Message: fmt.Sprintf("Selector scoring %d sketches against rubric...", len(sketches)),
			})
			verdict, serr := o.selector.Run(llm.WithGate(ctx, "selector"), o.ctx)
			if serr != nil {
				// Selector failure is non-fatal: log and keep going
				// to sketches_ready so the user can pick manually.
				fmt.Fprintf(o.reporterLog, "aidev selector: %v (falling back to manual sketch pick)\n", serr)
			} else {
				o.ctx.Selector = verdict
				msg := fmt.Sprintf("Selector chose Sketch %d (score %.2f)", verdict.ChosenNumber, verdict.Score)
				if verdict.ChosenNumber == 0 {
					msg = "Selector found no implementable sketch — needs refinement"
				}
				if verdict.TieBreakerUsed {
					msg += " (tie-breaker invoked)"
				}
				o.emit(ctx, out, Event{State: o.state, Message: msg})
			}
		}

		// Refinement detection (P5): if the Selector ran and refused
		// to pick (chosen=0), the pipeline cannot proceed
		// autonomously. Transition to StateNeedsRefinement so the
		// reporter posts the structured "needs refinement" comment
		// and the slash command stops without filing a PR.
		if o.ctx.Selector != nil && o.ctx.Selector.ChosenNumber == 0 {
			o.state = StateNeedsRefinement
			o.emit(ctx, out, Event{
				State:   o.state,
				Message: o.ctx.Selector.Rationale,
			})
			return
		}

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
