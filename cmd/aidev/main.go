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
	"github.com/aisu-ai/aidev/internal/orchestrator"
	"github.com/aisu-ai/aidev/internal/tui"
)

func main() {
	var (
		issueURL  = flag.String("issue", "", "GitHub issue URL (https://github.com/owner/repo/issues/123)")
		repoPath  = flag.String("repo", ".", "Path to the target repository")
		configDir = flag.String("config", "", "Path to aidev config directory (defaults to ./config or $AIDEV_CONFIG)")
		headless  = flag.Bool("headless", false, "Run the full pipeline once and print the report to stdout without the TUI")
		sketchN   = flag.Int("n", agents.DefaultSketchCount, "Number of Architect sketches to produce when the Critic recommends 'build'")
		autoRun   = flag.Bool("auto", false, "Headless only: automatically run the Architect when the Critic recommends 'build' (otherwise stop at Critic)")
	)
	flag.Parse()

	if *issueURL == "" {
		fatal("missing required flag: -issue")
	}

	cfgDir := *configDir
	if cfgDir == "" {
		cfgDir = os.Getenv("AIDEV_CONFIG")
	}
	if cfgDir == "" {
		// Prefer config relative to the binary location so `aidev` works
		// from any CWD after installation.
		if exe, err := os.Executable(); err == nil {
			candidate := filepath.Join(filepath.Dir(exe), "config")
			if _, err := os.Stat(candidate); err == nil {
				cfgDir = candidate
			}
		}
	}
	if cfgDir == "" {
		cfgDir = "config"
	}

	cfg, err := config.Load(cfgDir)
	if err != nil {
		fatal(fmt.Sprintf("load config: %v", err))
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

	if *headless {
		runHeadless(ctx, orch, *autoRun)
		return
	}

	model := tui.New(orch, *issueURL, absRepo)
	p := tea.NewProgram(model, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fatal(fmt.Sprintf("tui: %v", err))
	}
}

// runHeadless is a CI-friendly mode that produces a single Markdown report
// on stdout. With -auto, it also runs the Architect when the Critic
// recommends "build", so a CI pipeline can get the sketches in one pass.
func runHeadless(ctx context.Context, orch *orchestrator.Orchestrator, auto bool) {
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
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, "aidev: "+msg)
	os.Exit(1)
}
