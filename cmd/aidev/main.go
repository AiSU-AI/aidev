// Command aidev is a local, multi-agent coding assistant TUI.
//
// v0.1 scope: feed it a GitHub issue URL and a target repository path; it
// runs a Scout (local) and Critic (cloud) pass and renders their output in
// a three-pane Bubble Tea interface, culminating in a build/defer/kill
// recommendation that awaits your verdict.
//
// v0.2a adds the Architect: once the Critic recommends "build" and the
// human approves, the Architect produces N divergent solution sketches for
// the developer to pick from before the Implementer (v0.3) writes any code.
//
// v0.2b adds `aidev doctor` and automatic precondition checking on startup:
// Ollama binary presence, daemon health (auto-spawned when missing),
// required models (with an interactive pull/swap/abort prompt), plus
// config and Anthropic key validation.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/aisu-ai/aidev/internal/agents"
	"github.com/aisu-ai/aidev/internal/config"
	"github.com/aisu-ai/aidev/internal/doctor"
	"github.com/aisu-ai/aidev/internal/llm"
	"github.com/aisu-ai/aidev/internal/orchestrator"
	"github.com/aisu-ai/aidev/internal/tui"
)

func main() {
	// Intercept the `doctor` subcommand before flag parsing so it can
	// share the normal config discovery but not require -issue/-repo.
	if len(os.Args) > 1 && os.Args[1] == "doctor" {
		os.Args = append(os.Args[:1], os.Args[2:]...)
		runDoctorSubcommand()
		return
	}
	// Intercept the `charter` subcommand. Interactive interview that
	// writes .aidev/charter.md to a target repo.
	if len(os.Args) > 1 && os.Args[1] == "charter" {
		os.Args = append(os.Args[:1], os.Args[2:]...)
		runCharterSubcommand()
		return
	}

	var (
		issueURL     = flag.String("issue", "", "GitHub issue URL (https://github.com/owner/repo/issues/123)")
		repoPath     = flag.String("repo", ".", "Path to the target repository")
		configDir    = flag.String("config", "", "Path to aidev config directory (defaults to ./config or $AIDEV_CONFIG)")
		headless     = flag.Bool("headless", false, "Run the full pipeline once and print the report to stdout without the TUI")
		sketchN      = flag.Int("n", agents.DefaultSketchCount, "Number of Architect sketches to produce when the Critic recommends 'build'")
		pickSketch   = flag.Int("sketch", 0, "Headless only: after Architect produces sketches, automatically run the Implementer on this sketch number (1-indexed). 0 disables.")
		autoRun      = flag.Bool("auto", false, "Headless only: automatically run the Architect when the Critic recommends 'build' (otherwise stop at Critic)")
		skipDoctor   = flag.Bool("skip-doctor", false, "Skip the startup precondition check (not recommended)")
		noAuditTrail = flag.Bool("no-audit-trail", false, "Disable posting aidev progress + artifacts to the GitHub issue")
	)
	flag.Parse()

	if *issueURL == "" {
		fatal("missing required flag: -issue")
	}

	cfg := mustLoadConfig(*configDir)

	// Precondition audit. In headless mode the doctor never prompts —
	// it just reports and either fails or passes. In TTY mode we enable
	// auto-spawn and interactive fixes so the user can resolve a missing
	// daemon or missing model without leaving the program.
	if !*skipDoctor {
		opts := doctor.Options{
			Interactive:     !*headless && doctor.IsTTY(),
			AutoSpawnOllama: !*headless && doctor.IsTTY(),
			Out:             os.Stderr,
		}
		report := doctor.Run(context.Background(), cfg, opts)
		if report.HasFailure() {
			report.Write(os.Stderr)
			fatal("precondition checks failed — run `aidev doctor` for details")
		}
	}

	orch, err := orchestrator.New(cfg, orchestrator.WithSketchCount(*sketchN))
	if err != nil {
		fatal(fmt.Sprintf("orchestrator: %v", err))
	}

	ctx := context.Background()
	if err := orch.LoadIssue(ctx, *issueURL); err != nil {
		fatal(fmt.Sprintf("load issue: %v", err))
	}
	absRepo, err := filepath.Abs(*repoPath)
	if err != nil {
		fatal(fmt.Sprintf("resolve repo path: %v", err))
	}
	if err := orch.LoadRepo(absRepo); err != nil {
		fatal(fmt.Sprintf("scan repo: %v", err))
	}

	// Nudge the user if the repo has no strong signal for the Critic to
	// anchor against. The Critic will still run (it's more lenient than
	// this check) but the quality of its reasoning drops sharply without
	// a product charter.
	if snap := orch.AgentContext().Snapshot; snap != nil && !snap.HasStrongSignal() {
		fmt.Fprintln(os.Stderr, "warning: target repo has no README, CLAUDE.md, ARCHITECTURE.md, or .aidev/charter.md.")
		fmt.Fprintln(os.Stderr, "         consider running `aidev charter -repo "+absRepo+"` to establish the product's purpose before running the full pipeline.")
	}

	// Install the GitHubReporter unless the user opted out. We hand it
	// the same github.Client the orchestrator already uses, so the
	// credential story is "one GITHUB_TOKEN env var covers everything".
	if !*noAuditTrail {
		if issue := orch.AgentContext().Issue; issue != nil {
			reporter := orchestrator.NewGitHubReporter(ctx, orch.GitHubClient(), issue, os.Stderr)
			orch.SetReporter(reporter)
			defer func() { _ = orch.CloseReporter() }()
		}
	}

	if *headless {
		runHeadless(ctx, orch, *autoRun, *pickSketch)
		return
	}

	model := tui.New(orch, *issueURL, absRepo)
	p := tea.NewProgram(model, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fatal(fmt.Sprintf("tui: %v", err))
	}
}

// runCharterSubcommand handles `aidev charter`. It runs the interview
// flow against a target repo (default cwd) and writes the resulting
// markdown to `<repo>/.aidev/charter.md`. The subcommand never runs the
// agent pipeline — it is a one-shot tool.
func runCharterSubcommand() {
	var (
		repoPath  = flag.String("repo", ".", "Path to the target repository")
		configDir = flag.String("config", "", "Path to aidev config directory (defaults to ./config or $AIDEV_CONFIG)")
	)
	flag.Parse()

	cfg := mustLoadConfig(*configDir)

	router, err := llm.NewRouter(cfg)
	if err != nil {
		fatal(fmt.Sprintf("router: %v", err))
	}
	charter, err := agents.NewCharter(router, nil)
	if err != nil {
		fatal(fmt.Sprintf("charter: %v", err))
	}
	absRepo, err := filepath.Abs(*repoPath)
	if err != nil {
		fatal(fmt.Sprintf("resolve repo path: %v", err))
	}

	fmt.Fprintln(os.Stderr, "aidev charter: interactive interview. Answer each question, then press enter.")
	fmt.Fprintln(os.Stderr, "Leaving an optional field blank is fine.")
	fmt.Fprintln(os.Stderr)

	ctx := context.Background()
	path, err := charter.Interview(ctx, absRepo)
	if err != nil {
		fatal(fmt.Sprintf("charter: %v", err))
	}
	fmt.Fprintf(os.Stderr, "\nwrote %s\n", path)
}

// runDoctorSubcommand handles `aidev doctor`. It always runs non-interactive
// (no auto-spawn, no prompts) and prints the full report. Exit code is 0
// when there are no FAILs, 1 otherwise.
func runDoctorSubcommand() {
	var (
		configDir = flag.String("config", "", "Path to aidev config directory (defaults to ./config or $AIDEV_CONFIG)")
	)
	flag.Parse()

	cfg := mustLoadConfig(*configDir)

	opts := doctor.Options{
		Interactive:     false,
		AutoSpawnOllama: false,
		Out:             os.Stderr,
	}
	report := doctor.Run(context.Background(), cfg, opts)
	report.Write(os.Stdout)
	if report.HasFailure() {
		os.Exit(1)
	}
}

// mustLoadConfig resolves the config directory from flag, env var, and
// binary-relative defaults, then loads it or fatals.
func mustLoadConfig(configDir string) *config.Config {
	if configDir == "" {
		configDir = os.Getenv("AIDEV_CONFIG")
	}
	if configDir == "" {
		// Prefer config relative to the binary so `aidev` works from any
		// CWD after installation.
		if exe, err := os.Executable(); err == nil {
			candidate := filepath.Join(filepath.Dir(exe), "config")
			if _, err := os.Stat(candidate); err == nil {
				configDir = candidate
			}
		}
	}
	if configDir == "" {
		configDir = "config"
	}

	cfg, err := config.Load(configDir)
	if err != nil {
		fatal(fmt.Sprintf("load config: %v", err))
	}
	return cfg
}

// runHeadless is a CI-friendly mode that produces a single Markdown report
// on stdout. With -auto, it also runs the Architect when the Critic
// recommends "build", so a CI pipeline can get the sketches in one pass.
// With -sketch N, it additionally runs the Implementer against the chosen
// sketch and prints the resulting patch.
func runHeadless(ctx context.Context, orch *orchestrator.Orchestrator, auto bool, pickSketch int) {
	for ev := range orch.Run(ctx) {
		if ev.Err != nil {
			fatal(ev.Err.Error())
		}
	}
	agentCtx := orch.AgentContext()
	rpt := orch.CriticReport()

	fmt.Println("# aidev headless report")
	fmt.Println()
	if agentCtx.Issue != nil {
		fmt.Printf("Issue: %s/%s#%d — %s\n\n", agentCtx.Issue.Owner, agentCtx.Issue.Repo, agentCtx.Issue.Number, agentCtx.Issue.Title)
	}
	fmt.Println("## Scout brief")
	fmt.Println()
	fmt.Println(agentCtx.ScoutReport)
	fmt.Println()
	fmt.Println("## Critic report")
	fmt.Println()
	if rpt != nil {
		fmt.Println(rpt.Markdown)
		fmt.Println()
		fmt.Printf("RECOMMENDATION: %s\n", rpt.Recommendation)
	}

	if !auto || rpt == nil || rpt.Recommendation != "build" {
		return
	}

	// Auto-run the Architect because the Critic said build.
	preview := orch.CostPreview()
	fmt.Println()
	fmt.Printf("## Architect (auto, %d sketches, ~%d tokens, ~%ds)\n\n",
		preview.N, preview.TotalOutputTokens, preview.EstimatedSeconds)
	for ev := range orch.Continue(ctx) {
		if ev.Err != nil {
			fatal(ev.Err.Error())
		}
	}
	for i, s := range orch.AgentContext().Sketches {
		if i > 0 {
			fmt.Println()
			fmt.Println("---")
			fmt.Println()
		}
		fmt.Println(s.Markdown)
	}

	// Implementer opt-in.
	if pickSketch <= 0 {
		return
	}
	fmt.Println()
	fmt.Printf("## Implementer (auto, sketch %d)\n\n", pickSketch)
	for ev := range orch.Implement(ctx, pickSketch) {
		if ev.Err != nil {
			fatal(ev.Err.Error())
		}
	}
	if p := orch.Patch(); p != nil {
		fmt.Println("```diff")
		fmt.Println(p.Diff)
		fmt.Println("```")
		if p.Path != "" {
			fmt.Printf("\n_Patch also written to %s_\n", p.Path)
		}
	}
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, "aidev: "+msg)
	os.Exit(1)
}
