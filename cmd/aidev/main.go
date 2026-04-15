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
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/aisu-ai/aidev/internal/agents"
	"github.com/aisu-ai/aidev/internal/completion"
	"github.com/aisu-ai/aidev/internal/config"
	"github.com/aisu-ai/aidev/internal/doctor"
	"github.com/aisu-ai/aidev/internal/github"
	"github.com/aisu-ai/aidev/internal/installpkg"
	"github.com/aisu-ai/aidev/internal/llm"
	"github.com/aisu-ai/aidev/internal/orchestrator"
	"github.com/aisu-ai/aidev/internal/plugin"
	"github.com/aisu-ai/aidev/internal/tui"
	"github.com/aisu-ai/aidev/internal/version"
)

func main() {
	// --version / -V is a zero-dependency query, cheap to handle
	// before any flag parsing or subcommand dispatch.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--version", "-V", "version":
			fmt.Printf("aidev %s\n", version.Version)
			return
		}
	}
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
	// Intercept the `test` subcommand. Detects the project's test
	// runner and executes it against the current working tree.
	if len(os.Args) > 1 && os.Args[1] == "test" {
		os.Args = append(os.Args[:1], os.Args[2:]...)
		runTestSubcommand()
		return
	}
	// Intercept the `review` subcommand. Runs the Reviewer against a
	// patch file (default: .aidev/proposed.patch in the target repo)
	// and prints the review plus any follow-up proposals.
	if len(os.Args) > 1 && os.Args[1] == "review" {
		os.Args = append(os.Args[:1], os.Args[2:]...)
		runReviewSubcommand()
		return
	}
	// Intercept the `plugin` subcommand. Installs/uninstalls aidev's
	// slash commands into the user's Claude Code config directory.
	if len(os.Args) > 1 && os.Args[1] == "plugin" {
		os.Args = append(os.Args[:1], os.Args[2:]...)
		runPluginSubcommand()
		return
	}
	// Intercept the `install` subcommand. Writes the shipped default
	// config files into ~/.config/aidev (or $XDG_CONFIG_HOME/aidev)
	// so the installed binary can find them without AIDEV_CONFIG.
	if len(os.Args) > 1 && os.Args[1] == "install" {
		os.Args = append(os.Args[:1], os.Args[2:]...)
		runInstallSubcommand()
		return
	}
	// Intercept the `followups` subcommand. Reads .aidev/followups.md,
	// parses the Reviewer's proposals, and files them as real GitHub
	// issues (when --file-issues is passed).
	if len(os.Args) > 1 && os.Args[1] == "followups" {
		os.Args = append(os.Args[:1], os.Args[2:]...)
		runFollowUpsSubcommand()
		return
	}
	// Intercept the `clarify` subcommand. Runs the Clarifier agent
	// against an issue + fresh Critic report, walks the resulting
	// question graph in dependency-aware waves, and writes the
	// session to .aidev/clarifier.md for the Architect to absorb on
	// its next run.
	if len(os.Args) > 1 && os.Args[1] == "clarify" {
		os.Args = append(os.Args[:1], os.Args[2:]...)
		runClarifySubcommand()
		return
	}
	// Intercept the `config` subcommand tree (v0.5). Single edit
	// surface for models.yaml that both humans and Claude Code
	// sessions can drive — replaces the legacy --preset flag and
	// per-file preset machinery.
	if len(os.Args) > 1 && os.Args[1] == "config" {
		os.Args = append(os.Args[:1], os.Args[2:]...)
		runConfigSubcommand()
		return
	}
	// Intercept the `ollama` subcommand. Provides model management,
	// recommendations, and health checks for the local Ollama daemon.
	if len(os.Args) > 1 && os.Args[1] == "ollama" {
		os.Args = append(os.Args[:1], os.Args[2:]...)
		runOllamaSubcommand()
		return
	}
	// Intercept the `completion` subcommand. Installs shell completion
	// for bash and zsh to enable tab autocompletion.
	if len(os.Args) > 1 && os.Args[1] == "completion" {
		// Check for script output mode - don't modify args
		if len(os.Args) > 3 && os.Args[3] == "--script" {
			// Don't modify os.Args, just call the subcommand directly
			runCompletionSubcommandWithArgs(os.Args[2:])
			return
		}
		// Check for help flags before modifying args
		if len(os.Args) > 2 && (os.Args[2] == "--help" || os.Args[2] == "-h") {
			// Don't modify os.Args, just call the subcommand directly
			runCompletionSubcommandWithArgs(os.Args[2:])
			return
		}
		os.Args = append(os.Args[:1], os.Args[2:]...)
		runCompletionSubcommand()
		return
	}

	var (
		issueURL     = flag.String("issue", "", "GitHub issue reference: full URL (https://github.com/owner/repo/issues/N) or a bare number when -repo points at a local clone with a github.com remote")
		repoPath     = flag.String("repo", ".", "Path to the target repository")
		configDir    = flag.String("config", "", "Path to aidev config directory (defaults to ./config or $AIDEV_CONFIG)")
		headless     = flag.Bool("headless", false, "Run the full pipeline once and print the report to stdout without the TUI")
		sketchN      = flag.Int("n", agents.DefaultSketchCount, "Number of Architect sketches to produce when the Critic recommends 'build'")
		pickSketch   = flag.Int("sketch", 0, "Headless only: after Architect produces sketches, automatically run the Implementer on this sketch number (1-indexed). 0 disables.")
		autoRun      = flag.Bool("auto", false, "Headless only: automatically run the Architect when the Critic recommends 'build' (otherwise stop at Critic)")
		forceVerdict = flag.String("force-verdict", "", "Headless only: ESCAPE HATCH. After running Scout+Critic (and any interview rounds), override the Critic's verdict with this value: build|defer|kill. Use ONLY when you have answered the Critic's questions and the Critic is still hedging. Always logged loudly so you can see when the override fired.")
		skipDoctor   = flag.Bool("skip-doctor", false, "Skip the startup precondition check (not recommended)")
		noAuditTrail = flag.Bool("no-audit-trail", false, "Disable posting aidev progress + artifacts to the GitHub issue")
	)
	flag.Parse()

	if *issueURL == "" {
		fatal("missing required flag: -issue")
	}

	cfg := mustLoadConfig(*configDir)
	printStartupBanner(cfg)

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

	// Resolve the repo path FIRST because the issue resolver may need
	// it to turn a bare issue number into a full URL via git remote
	// inference. Repo-less bare numbers fail loudly instead of silently
	// running against the wrong target.
	absRepo, err := filepath.Abs(*repoPath)
	if err != nil {
		fatal(fmt.Sprintf("resolve repo path: %v", err))
	}

	resolvedIssueURL, _, _, _, err := github.ResolveIssueRef(*issueURL, absRepo)
	if err != nil {
		fatal(fmt.Sprintf("resolve issue: %v", err))
	}

	ctx := context.Background()
	if err := orch.LoadIssue(ctx, resolvedIssueURL); err != nil {
		fatal(fmt.Sprintf("load issue: %v", err))
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
		// Validate -force-verdict early so a typo fails fast instead of
		// silently being treated as "no override". Empty string means
		// the flag isn't set; otherwise it must be a recognised verdict.
		switch *forceVerdict {
		case "", "build", "defer", "kill":
			// ok
		default:
			fatal(fmt.Sprintf("invalid -force-verdict %q (must be build, defer, or kill)", *forceVerdict))
		}
		runHeadless(ctx, orch, cfg, absRepo, *autoRun, *pickSketch, *forceVerdict)
		return
	}

	model := tui.New(orch, *issueURL, absRepo)
	p := tea.NewProgram(model, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fatal(fmt.Sprintf("tui: %v", err))
	}
}

// runClarifySubcommand handles `aidev clarify`. It runs the full
// Scout + Critic pipeline so the Clarifier has context to work from,
// then asks the Clarifier to produce a structured question graph,
// then walks the graph in dependency-aware waves with stdin-based
// answers from the developer, and finally persists the session to
// `<repo>/.aidev/clarifier.md`.
//
// Unlike the full `aidev -issue ...` flow this subcommand does NOT
// run the Architect afterwards — it's a standalone interview you run
// BEFORE the Architect when the Critic flagged ambiguities.
func runClarifySubcommand() {
	var (
		issueURL  = flag.String("issue", "", "GitHub issue reference: full URL or a bare number (resolved against -repo's git remote)")
		repoPath  = flag.String("repo", ".", "Path to the target repository")
		configDir = flag.String("config", "", "Path to aidev config directory (defaults to ./config or $AIDEV_CONFIG)")
	)
	flag.Parse()
	if *issueURL == "" {
		fatal("missing required flag: -issue")
	}

	cfg := mustLoadConfig(*configDir)
	orch, err := orchestrator.New(cfg)
	if err != nil {
		fatal(fmt.Sprintf("orchestrator: %v", err))
	}
	absRepo, err := filepath.Abs(*repoPath)
	if err != nil {
		fatal(fmt.Sprintf("resolve repo: %v", err))
	}
	resolvedIssueURL, _, _, _, err := github.ResolveIssueRef(*issueURL, absRepo)
	if err != nil {
		fatal(fmt.Sprintf("resolve issue: %v", err))
	}
	ctx := context.Background()
	if err := orch.LoadIssue(ctx, resolvedIssueURL); err != nil {
		fatal(fmt.Sprintf("load issue: %v", err))
	}
	if err := orch.LoadRepo(absRepo); err != nil {
		fatal(fmt.Sprintf("scan repo: %v", err))
	}

	// Run the first-pass pipeline so the agent context has a fresh
	// Scout brief + Critic report to hand to the Clarifier.
	fmt.Fprintln(os.Stderr, "aidev clarify: running scout + critic to gather context...")
	for ev := range orch.Run(ctx) {
		if ev.Err != nil {
			fatal(ev.Err.Error())
		}
	}

	router, err := llm.NewRouter(cfg)
	if err != nil {
		fatal(fmt.Sprintf("router: %v", err))
	}
	clarifier, err := agents.NewClarifier(router)
	if err != nil {
		fatal(fmt.Sprintf("clarifier: %v", err))
	}

	fmt.Fprintln(os.Stderr, "aidev clarify: asking the Clarifier to produce a question graph...")
	graph, err := clarifier.Run(ctx, orch.AgentContext())
	if err != nil {
		fatal(fmt.Sprintf("clarifier: %v", err))
	}
	if len(graph.Questions) == 0 {
		fmt.Fprintln(os.Stderr, "Clarifier produced no questions — the Critic report has nothing ambiguous worth asking about. Skipping the interview.")
		return
	}

	// Walk the graph in waves.
	waves := agents.BatchByDependencies(graph)
	var answers []agents.Answer
	reader := bufio.NewReader(os.Stdin)

	fmt.Fprintf(os.Stderr, "\n%d questions in %d wave(s). Answer each one, then press enter.\n\n", len(graph.Questions), len(waves))
	for wi, wave := range waves {
		fmt.Fprintf(os.Stderr, "— Wave %d (%d question(s)) —\n\n", wi+1, len(wave))
		for _, q := range wave {
			fmt.Fprintf(os.Stderr, "%s: %s\n> ", q.ID, q.Text)
			line, err := reader.ReadString('\n')
			if err != nil && line == "" {
				fatal(fmt.Sprintf("read answer: %v", err))
			}
			answers = append(answers, agents.Answer{
				ID:   q.ID,
				Text: strings.TrimRight(line, "\n"),
			})
		}
		fmt.Fprintln(os.Stderr)
	}

	path, err := agents.WriteClarifierMarkdown(absRepo, graph, answers)
	if err != nil {
		fatal(fmt.Sprintf("write clarifier: %v", err))
	}
	fmt.Fprintf(os.Stderr, "wrote %s\n", path)
}

// runFollowUpsSubcommand handles `aidev followups`. With --file-issues,
// it reads the Reviewer-produced .aidev/followups.md file, parses each
// proposal, and files them as real GitHub issues on the specified
// target repo. Without --file-issues, it lists what WOULD be filed —
// a dry run. Either way, the user is always in control of when
// followups turn into issues.
func runFollowUpsSubcommand() {
	var (
		repoPath  = flag.String("repo", ".", "Path to the target repository (where .aidev/followups.md lives)")
		target    = flag.String("target", "", "Target GitHub repo in owner/repo form (required for --file-issues)")
		file      = flag.Bool("file-issues", false, "Actually file the proposals as GitHub issues (default: dry run)")
		followups = flag.String("path", "", "Override path to followups.md (default: <repo>/.aidev/followups.md)")
		configDir = flag.String("config", "", "Path to aidev config directory (defaults to ./config or $AIDEV_CONFIG)")
	)
	flag.Parse()

	_ = mustLoadConfig(*configDir) // validate config even though we don't use it directly

	absRepo, err := filepath.Abs(*repoPath)
	if err != nil {
		fatal(fmt.Sprintf("resolve repo: %v", err))
	}
	path := *followups
	if path == "" {
		path = filepath.Join(absRepo, ".aidev", "followups.md")
	}

	proposals, err := agents.ParseFollowUpsFile(path)
	if err != nil {
		fatal(fmt.Sprintf("parse followups: %v", err))
	}
	if len(proposals) == 0 {
		fmt.Fprintf(os.Stderr, "no follow-ups found at %s\n", path)
		return
	}

	if !*file {
		// Dry run — list the proposals.
		fmt.Printf("# Dry run — %d proposal(s) at %s\n\n", len(proposals), path)
		fmt.Printf("_Pass --file-issues --target owner/repo to actually create these on GitHub._\n\n")
		for i, p := range proposals {
			fmt.Printf("## %d. %s\n\n", i+1, p.Title)
			if len(p.Labels) > 0 {
				fmt.Printf("labels: %s\n\n", strings.Join(p.Labels, ", "))
			}
			fmt.Println(p.Body)
			fmt.Println()
		}
		return
	}

	if *target == "" {
		fatal("--file-issues requires --target owner/repo")
	}
	parts := strings.SplitN(*target, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		fatal(fmt.Sprintf("invalid --target %q, want owner/repo form", *target))
	}
	owner, repo := parts[0], parts[1]

	client := github.NewClient()
	ctx := context.Background()

	filed, err := agents.FileFollowUps(proposals, func(title, body string, labels []string) (int, string, error) {
		return client.CreateIssue(ctx, owner, repo, title, body, labels)
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "\nerror partway through filing: %v\n", err)
		fmt.Fprintf(os.Stderr, "filed %d issue(s) before the failure:\n", len(filed))
	}
	for _, f := range filed {
		fmt.Printf("filed #%d: %s — %s\n", f.Number, f.Proposal.Title, f.URL)
	}
	if err != nil {
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "\n%d issue(s) filed against %s/%s\n", len(filed), owner, repo)
}

// runInstallSubcommand handles `aidev install`. Writes the shipped
// default config (models.yaml, principles.yaml) into
// $XDG_CONFIG_HOME/aidev (or ~/.config/aidev) so the installed binary
// can find them without the user needing to set AIDEV_CONFIG or
// symlink anything. Idempotent: existing files are preserved unless
// --force is passed.
//
// This is the config half of the full install flow. The binary and
// plugin halves are handled by the top-level install.sh script which
// calls `go build`, moves the binary to a bin dir, then invokes this
// subcommand and `aidev plugin install`.
func runInstallSubcommand() {
	var (
		force = flag.Bool("force", false, "Overwrite existing config files")
		dir   = flag.String("dir", "", "Target directory (default: $XDG_CONFIG_HOME/aidev or ~/.config/aidev)")
	)
	flag.Parse()

	target := *dir
	if target == "" {
		resolved, err := installpkg.DefaultConfigDir()
		if err != nil {
			fatal(fmt.Sprintf("install: %v", err))
		}
		target = resolved
	}

	written, skipped, err := installpkg.InstallConfig(target, *force)
	if err != nil {
		fatal(fmt.Sprintf("install: %v", err))
	}
	for _, p := range written {
		fmt.Fprintf(os.Stderr, "wrote: %s\n", p)
	}
	for _, p := range skipped {
		fmt.Fprintf(os.Stderr, "skipped (exists): %s\n", p)
	}
	fmt.Fprintf(os.Stderr, "\n%d written, %d skipped\n", len(written), len(skipped))
	if len(skipped) > 0 {
		fmt.Fprintln(os.Stderr, "pass --force to overwrite skipped files")
	}
	fmt.Fprintln(os.Stderr)
	fmt.Fprintf(os.Stderr, "config installed at %s\n", target)
	fmt.Fprintln(os.Stderr, "aidev will now find its config automatically from any cwd.")
	fmt.Fprintln(os.Stderr, "next: run `aidev plugin install` to install the Claude Code slash commands.")
}

// runPluginSubcommand handles `aidev plugin install | uninstall`.
// Install copies aidev's slash commands into the user's Claude Code
// commands directory (~/.claude/commands by default, overridable via
// CLAUDE_CONFIG_DIR). Existing files are preserved unless --force is
// passed. Uninstall removes files whose content matches the shipped
// version, preserving any files the user edited locally.
func runPluginSubcommand() {
	if len(os.Args) < 2 {
		fatal("usage: aidev plugin install|uninstall [--force] [--dir <path>]")
	}
	action := os.Args[1]
	os.Args = append(os.Args[:1], os.Args[2:]...)

	var (
		force = flag.Bool("force", false, "Overwrite existing files on install")
		dir   = flag.String("dir", "", "Target directory (default: ~/.claude/commands)")
	)
	flag.Parse()

	target := *dir
	if target == "" {
		resolved, err := plugin.DefaultCommandsDir()
		if err != nil {
			fatal(fmt.Sprintf("plugin: %v", err))
		}
		target = resolved
	}

	switch action {
	case "install":
		written, skipped, err := plugin.Install(target, *force)
		if err != nil {
			fatal(fmt.Sprintf("plugin install: %v", err))
		}
		for _, p := range written {
			fmt.Fprintf(os.Stderr, "installed: %s\n", p)
		}
		for _, p := range skipped {
			fmt.Fprintf(os.Stderr, "skipped (exists): %s\n", p)
		}
		fmt.Fprintf(os.Stderr, "\n%d installed, %d skipped\n", len(written), len(skipped))
		if len(skipped) > 0 {
			fmt.Fprintln(os.Stderr, "pass --force to overwrite skipped files")
		}
	case "uninstall":
		removed, err := plugin.Uninstall(target)
		if err != nil {
			fatal(fmt.Sprintf("plugin uninstall: %v", err))
		}
		for _, p := range removed {
			fmt.Fprintf(os.Stderr, "removed: %s\n", p)
		}
		fmt.Fprintf(os.Stderr, "\n%d removed (locally-modified files preserved)\n", len(removed))
	default:
		fatal(fmt.Sprintf("plugin: unknown action %q (use install or uninstall)", action))
	}
}

// runReviewSubcommand handles `aidev review`. It loads a patch file
// (default `<repo>/.aidev/proposed.patch`), fetches the issue context,
// runs the Reviewer, and prints the structured review to stdout. If the
// review contains follow-up proposals they're also written to
// `<repo>/.aidev/followups.md` for manual triage.
func runReviewSubcommand() {
	var (
		issueURL  = flag.String("issue", "", "GitHub issue reference: full URL or a bare number (resolved against -repo's git remote)")
		repoPath  = flag.String("repo", ".", "Path to the target repository")
		patchPath = flag.String("patch", "", "Path to the patch file to review (default: <repo>/.aidev/proposed.patch)")
		configDir = flag.String("config", "", "Path to aidev config directory (defaults to ./config or $AIDEV_CONFIG)")
	)
	flag.Parse()
	if *issueURL == "" {
		fatal("missing required flag: -issue")
	}

	cfg := mustLoadConfig(*configDir)
	orch, err := orchestrator.New(cfg)
	if err != nil {
		fatal(fmt.Sprintf("orchestrator: %v", err))
	}
	absRepo, err := filepath.Abs(*repoPath)
	if err != nil {
		fatal(fmt.Sprintf("resolve repo: %v", err))
	}
	resolvedIssueURL, _, _, _, err := github.ResolveIssueRef(*issueURL, absRepo)
	if err != nil {
		fatal(fmt.Sprintf("resolve issue: %v", err))
	}
	ctx := context.Background()
	if err := orch.LoadIssue(ctx, resolvedIssueURL); err != nil {
		fatal(fmt.Sprintf("load issue: %v", err))
	}
	if err := orch.LoadRepo(absRepo); err != nil {
		fatal(fmt.Sprintf("scan repo: %v", err))
	}

	pp := *patchPath
	if pp == "" {
		pp = filepath.Join(absRepo, ".aidev", "proposed.patch")
	}
	data, err := os.ReadFile(pp)
	if err != nil {
		fatal(fmt.Sprintf("read patch %s: %v", pp, err))
	}

	for ev := range orch.ReviewPatch(ctx, string(data)) {
		if ev.Err != nil {
			fatal(ev.Err.Error())
		}
	}
	if rev := orch.Review(); rev != nil {
		fmt.Println(rev.Markdown)
	}
}

// runTestSubcommand handles `aidev test`. It detects the project's
// test runner, executes it against the working tree (or inside a
// Docker container if --sandbox is passed), and prints the result with
// an LLM-generated failure summary on non-zero exit.
func runTestSubcommand() {
	var (
		repoPath  = flag.String("repo", ".", "Path to the target repository")
		configDir = flag.String("config", "", "Path to aidev config directory (defaults to ./config or $AIDEV_CONFIG)")
		sandbox   = flag.Bool("sandbox", false, "Run tests inside a Docker container instead of on the host")
		image     = flag.String("image", "", "Docker image to use with --sandbox (e.g. golang:1.24, node:20, python:3.12)")
		writable  = flag.Bool("writable", false, "Mount the repo read-write instead of read-only (sandbox mode)")
	)
	flag.Parse()

	cfg := mustLoadConfig(*configDir)
	router, err := llm.NewRouter(cfg)
	if err != nil {
		fatal(fmt.Sprintf("router: %v", err))
	}
	tester, err := agents.NewTester(router)
	if err != nil {
		fatal(fmt.Sprintf("tester: %v", err))
	}

	if *sandbox {
		if *image == "" {
			fatal("--sandbox requires --image (e.g. --image golang:1.24)")
		}
		tester.SetSandbox(*image, *writable)
	}

	absRepo, err := filepath.Abs(*repoPath)
	if err != nil {
		fatal(fmt.Sprintf("resolve repo: %v", err))
	}

	if *sandbox {
		fmt.Fprintf(os.Stderr, "aidev test: running in sandbox (image %s, %s mount)...\n",
			*image, func() string {
				if *writable {
					return "rw"
				}
				return "ro"
			}())
	} else {
		fmt.Fprintf(os.Stderr, "aidev test: detecting test runner in %s...\n", absRepo)
	}
	result, err := tester.Run(context.Background(), absRepo)
	if err != nil {
		fatal(fmt.Sprintf("test: %v", err))
	}

	fmt.Printf("command: %s\n", result.Command)
	fmt.Printf("detected: %s\n", result.Detected)
	fmt.Printf("sandboxed: %t\n", result.Sandboxed)
	fmt.Printf("exit: %d\n", result.ExitCode)
	fmt.Printf("duration: %s\n", result.Duration)
	fmt.Printf("passed: %t\n\n", result.Passed)
	if result.Output != "" {
		fmt.Println("output (tail):")
		fmt.Println(result.Output)
		fmt.Println()
	}
	if result.Summary != "" {
		fmt.Println("failure summary:")
		fmt.Println(result.Summary)
	}
	if !result.Passed {
		os.Exit(1)
	}
}

// runCharterSubcommand handles `aidev charter`. It runs in one of two
// modes:
//
//   - Interactive (default): prompts the user for answers via stdin.
//     Fails fast if stdin isn't a TTY so we never hang waiting for
//     input that will never arrive.
//
//   - Non-interactive (`--answers-file <path>`): reads a JSON file
//     with the five answer fields and skips the interviewer
//     entirely. Used by the Claude Code `/aidev-charter` slash
//     command, which collects answers in the chat before invoking
//     aidev.
//
// Either way, the end result is a synthesised `<repo>/.aidev/charter.md`.
func runCharterSubcommand() {
	var (
		repoPath    = flag.String("repo", ".", "Path to the target repository")
		configDir   = flag.String("config", "", "Path to aidev config directory (defaults to ./config or $AIDEV_CONFIG)")
		answersFile = flag.String("answers-file", "", "Path to a JSON file containing the five charter answers (non-interactive mode). Keys: purpose, users, constraint, out_of_scope, overrides")
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

	ctx := context.Background()

	var path string
	if *answersFile != "" {
		// Non-interactive: load answers from JSON, skip the
		// interviewer.
		data, err := os.ReadFile(*answersFile)
		if err != nil {
			fatal(fmt.Sprintf("read answers file: %v", err))
		}
		var answers map[string]string
		if err := json.Unmarshal(data, &answers); err != nil {
			fatal(fmt.Sprintf("parse answers file: %v (expected a JSON object with string values)", err))
		}
		fmt.Fprintf(os.Stderr, "aidev charter: running in non-interactive mode with answers from %s\n", *answersFile)
		path, err = charter.RunWithAnswers(ctx, absRepo, answers)
		if err != nil {
			fatal(fmt.Sprintf("charter: %v", err))
		}
	} else {
		// Interactive: walk the user through the 5 questions.
		fmt.Fprintln(os.Stderr, "aidev charter: interactive interview. Answer each question, then press enter.")
		fmt.Fprintln(os.Stderr, "Leaving an optional field blank is fine. For non-interactive use, pass --answers-file.")
		fmt.Fprintln(os.Stderr)
		path, err = charter.Interview(ctx, absRepo)
		if err != nil {
			fatal(fmt.Sprintf("charter: %v", err))
		}
	}
	fmt.Fprintf(os.Stderr, "\nwrote %s\n", path)
}

// runDoctorSubcommand handles `aidev doctor`. It never prompts for user
// input (no missing-model menu, no pull consent), but it IS allowed to
// auto-spawn the Ollama daemon if the binary is present and the daemon
// isn't already running — spawning is idempotent, doesn't require a
// TTY, and the daemon persists after aidev exits so it's effectively
// the same as the user typing `ollama serve` in another terminal. Exit
// code is 0 when there are no FAILs, 1 otherwise.
func runDoctorSubcommand() {
	var (
		configDir = flag.String("config", "", "Path to aidev config directory (defaults to ./config or $AIDEV_CONFIG)")
		noSpawn   = flag.Bool("no-spawn", false, "Disable auto-spawning `ollama serve` if the daemon isn't running")
	)
	flag.Parse()

	cfg := mustLoadConfig(*configDir)

	opts := doctor.Options{
		Interactive:     false,
		AutoSpawnOllama: !*noSpawn,
		Out:             os.Stderr,
	}
	report := doctor.Run(context.Background(), cfg, opts)
	report.Write(os.Stdout)
	if report.HasFailure() {
		os.Exit(1)
	}
}

// mustLoadConfig resolves the config directory from (in order):
//
//  1. the explicit --config flag
//  2. the AIDEV_CONFIG env var
//  3. a `config` directory next to the binary (dev workflow — running
//     the built binary from the aidev checkout)
//  4. $XDG_CONFIG_HOME/aidev or ~/.config/aidev (installed workflow —
//     the installer script drops the shipped defaults here)
//  5. ~/.aidev/config (legacy home-dotdir fallback, for users who
//     prefer ~/.aidev over ~/.config/aidev)
//  6. ./config (cwd — matches the dev workflow for "go run")
//
// The first directory that exists and contains models.yaml wins.
//
// v0.5: the legacy --preset flag and mustLoadConfigPreset are gone.
// Profile selection now lives inside models.yaml itself via the
// active_profile field. Use `aidev config profile use <name>` to
// switch profiles instead of passing --preset.
func mustLoadConfig(configDir string) *config.Config {
	if configDir == "" {
		configDir = os.Getenv("AIDEV_CONFIG")
	}
	if configDir == "" {
		configDir = discoverConfigDir()
	}

	cfg, err := config.Load(configDir)
	if err != nil {
		fatal(fmt.Sprintf("load config: %v\n  searched: %s\n  hint: run `aidev install` to lay down the default config in ~/.config/aidev", err, configDir))
	}
	return cfg
}

// discoverConfigDir walks the candidate list and returns the first
// directory that contains models.yaml. Falls back to "config" (the
// bare-name cwd lookup) when nothing is found so config.Load can
// return a clean error.
func discoverConfigDir() string {
	candidates := []string{}

	// Binary-relative (dev workflow).
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "config"))
	}
	// XDG: $XDG_CONFIG_HOME/aidev with a ~/.config/aidev fallback.
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		candidates = append(candidates, filepath.Join(xdg, "aidev"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".config", "aidev"))
		candidates = append(candidates, filepath.Join(home, ".aidev", "config"))
	}
	// CWD — last resort so we don't accidentally shadow an installed config.
	candidates = append(candidates, "config")

	for _, c := range candidates {
		if _, err := os.Stat(filepath.Join(c, "models.yaml")); err == nil {
			return c
		}
	}
	return "config"
}

// printStartupBanner emits a one-line-per-role summary of the
// resolved active profile to stderr. Always runs at the top of any
// pipeline-driving subcommand so the user can see exactly which
// provider+model is wired to each agent role before the run starts.
//
// Critically, this also flags Implementer/Reviewer/Coordinator
// roles whose tier provider is `claude-cli` with a ⚠ marker,
// because that provider does NOT support tool use yet and the
// agent will silently fall back to the legacy NEED_FILES path.
// The user has been bitten by this enough times that explicit
// runtime visibility is mandatory.
func printStartupBanner(cfg *config.Config) {
	if cfg == nil || cfg.Models.Routing == nil {
		return
	}
	fmt.Fprintf(os.Stderr, "aidev: config=%s/models.yaml profile=%s\n", cfg.ConfigDir, cfg.ActiveProfile)
	// Stable role order for the banner — alphabetical inside each
	// group: agents first, then the coordinator overseer last.
	roleOrder := []string{
		"scout", "critic", "architect", "charter",
		"implementer", "reviewer", "tester",
		"coordinator",
	}
	for _, role := range roleOrder {
		tierName, ok := cfg.Models.Routing[role]
		if !ok {
			continue
		}
		tier, ok := cfg.Models.Tiers[tierName]
		if !ok {
			continue
		}
		marker := ""
		if (role == "implementer" || role == "reviewer") && tier.Provider == "claude-cli" {
			marker = "    ⚠ legacy NEED_FILES path (no tool use; swap to provider: ollama or anthropic for v0.4 tool use)"
		}
		modelDisplay := tier.Model
		if modelDisplay == "" {
			modelDisplay = "(default)"
		}
		// Show the effective per-call timeout next to each role
		// so the user knows how long a hung LLM call can block
		// the pipeline before the HTTP client gives up. This is
		// directly the UX answer to v0.5a's "32b timed out at 5
		// minutes with no visible warning" failure mode.
		timeoutDisplay := "20m0s (default)"
		if tier.TimeoutSeconds > 0 {
			timeoutDisplay = (time.Duration(tier.TimeoutSeconds) * time.Second).String()
		}
		fmt.Fprintf(os.Stderr, "  %-12s → %s:%s  (timeout=%s)%s\n", role, tier.Provider, modelDisplay, timeoutDisplay, marker)
	}
	fmt.Fprintln(os.Stderr)
}

// runHeadless is a CI-friendly mode that produces a single Markdown report
// on stdout. With -auto, it also runs the Architect when the Critic
// recommends "build", so a CI pipeline can get the sketches in one pass.
// With -sketch N, it additionally runs the Implementer against the chosen
// sketch and prints the resulting patch.
//
// When the Critic returns "unclear" or "defer" AND stdin is a TTY,
// runHeadless launches an in-line Clarifier interview instead of
// halting. It asks the Critic's own sharp questions via the Clarifier,
// collects stdin answers, writes them to .aidev/clarifier.md, and
// re-runs the Critic. Up to 2 interview rounds total — if the Critic
// still refuses, we halt and report the latest verdict. A "kill"
// verdict always halts without an interview; killing is explicit.
//
// forceVerdict ("build" / "defer" / "kill" / "") is the escape hatch
// for cases where the Critic keeps hedging despite answered questions.
// When non-empty it overrides the Critic's recommendation AFTER the
// normal pipeline (and any interview rounds) have run, and the
// override is logged loudly so the user always sees it fired.
func runHeadless(ctx context.Context, orch *orchestrator.Orchestrator, cfg *config.Config, absRepo string, auto bool, pickSketch int, forceVerdict string) {
	// Emit a per-gate telemetry table at the end of every run. defer
	// covers normal returns; bail() below handles the fatal-exit path
	// (os.Exit bypasses defers, so we render just before exiting).
	defer renderTelemetry(orch)

	// bail is the fatal path for runHeadless. It renders telemetry FIRST
	// so cost/latency data is visible on the failing run — that's when
	// the user most needs to see which gate burned budget before dying.
	// Every fatal() call inside runHeadless should go through bail().
	bail := func(msg string) {
		renderTelemetry(orch)
		fatal(msg)
	}

	for ev := range orch.Run(ctx) {
		if ev.Err != nil {
			bail(ev.Err.Error())
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

	// Interview loop on unclear/defer. Skipped when -auto is off
	// (caller is a CI pipeline, not a human), when there's no TTY on
	// stdin (headless without interactivity), or when the verdict is
	// build/kill (nothing to clarify).
	if auto && rpt != nil && doctor.IsTTY() {
		rpt = runInterviewLoop(ctx, orch, cfg, absRepo, rpt)
	}

	// Apply the user's escape-hatch override AFTER the natural Critic
	// + interview pipeline has run. We always run the Critic first so
	// the override is informed (you see what the Critic actually said
	// before you overrule it), and so the on-disk audit trail and any
	// downstream tooling that reads o.CriticReport() still has the
	// real verdict for context.
	if forceVerdict != "" && rpt != nil && rpt.Recommendation != forceVerdict {
		fmt.Println()
		fmt.Printf("## Verdict overridden by user\n\n")
		fmt.Printf("Critic recommended **%s**.\n", rpt.Recommendation)
		fmt.Printf("User passed `-force-verdict %s`. Proceeding as if the Critic had said %s.\n",
			forceVerdict, forceVerdict)
		fmt.Println()
		fmt.Println("This is the escape hatch. The Critic's report above is the audit trail; the override is your call and your responsibility.")
		rpt.Recommendation = forceVerdict
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
			bail(ev.Err.Error())
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
			bail(ev.Err.Error())
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

// renderTelemetry prints the per-gate token / latency summary table at
// the end of a headless run. It is deferred from runHeadless so it
// always fires, including on early returns (Critic verdict != build,
// unused sketch opt-in, etc.). Empty recorders render nothing so unit
// tests and offline dry-runs stay quiet.
func renderTelemetry(orch *orchestrator.Orchestrator) {
	if orch == nil {
		return
	}
	router := orch.Router()
	if router == nil {
		return
	}
	recorder := router.Recorder()
	summary := recorder.Summarize()
	if len(summary) == 0 {
		return
	}
	fmt.Println()
	fmt.Println("## Telemetry")
	fmt.Println()
	fmt.Println("| gate | provider | calls | input | output | elapsed | errors |")
	fmt.Println("| --- | --- | ---: | ---: | ---: | ---: | ---: |")
	var totInput, totOutput, totCalls, totErrors int
	var totElapsed time.Duration
	for _, s := range summary {
		fmt.Printf("| %s | %s | %d | %d | %d | %s | %d |\n",
			s.Gate, s.Provider, s.Calls, s.InputTokens, s.OutputTokens,
			s.Elapsed.Round(time.Millisecond), s.Errors)
		totCalls += s.Calls
		totInput += s.InputTokens
		totOutput += s.OutputTokens
		totElapsed += s.Elapsed
		totErrors += s.Errors
	}
	fmt.Printf("| **TOTAL** | | **%d** | **%d** | **%d** | **%s** | **%d** |\n",
		totCalls, totInput, totOutput, totElapsed.Round(time.Millisecond), totErrors)
	fmt.Println()
	fmt.Println("_Telemetry captures one row per pipeline gate. Retries are absorbed into `elapsed`; a gate with 2 retries shows 1 call and the full wall-clock including backoff._")
}

// runInterviewLoop drives up to maxInterviewRounds Clarifier
// interviews against a Critic that came back unclear/defer. Each round:
//
//  1. Run the Clarifier agent on the current Context to extract a
//     structured QuestionGraph from the Critic's own sharp questions.
//  2. Walk the graph in dependency-aware waves, collecting free-form
//     stdin answers from the developer.
//  3. Persist the session to .aidev/clarifier.md so downstream runs
//     pick it up via the Scout.
//  4. Stash the same markdown on ctx.ClarifierNotes and call
//     orch.Recritique(ctx) to get a fresh verdict.
//
// Returns the latest Critic report (which the caller compares against
// "build" to decide whether to proceed). Any fatal error along the
// way short-circuits via fatal() just like the rest of main.go — the
// user sees a clear message and the process exits.
//
// Design notes:
//
//   - kill is NEVER interviewed. A kill verdict is the Critic saying
//     "this is a bad idea, full stop". The interview is for
//     ambiguity, not for overriding rejection.
//
//   - build stops the loop immediately — no more questions to ask.
//
//   - If the Clarifier returns an empty question graph (nothing
//     ambiguous) the loop terminates: we have no way to move the
//     needle without inventing questions.
//
//   - Max 2 rounds. Past that, we print the latest verdict and let
//     the developer decide out-of-band whether to override.
const maxInterviewRounds = 2

func runInterviewLoop(
	ctx context.Context,
	orch *orchestrator.Orchestrator,
	cfg *config.Config,
	absRepo string,
	initial *agents.Report,
) *agents.Report {
	rpt := initial
	if rpt == nil {
		return nil
	}

	router, err := llm.NewRouter(cfg)
	if err != nil {
		fatal(fmt.Sprintf("router: %v", err))
	}
	clarifier, err := agents.NewClarifier(router)
	if err != nil {
		fatal(fmt.Sprintf("clarifier: %v", err))
	}
	reader := bufio.NewReader(os.Stdin)

	for round := 1; round <= maxInterviewRounds; round++ {
		if rpt.Recommendation == "build" || rpt.Recommendation == "kill" {
			return rpt
		}

		fmt.Println()
		fmt.Printf("## Clarifier interview (round %d of %d — Critic said %q)\n\n",
			round, maxInterviewRounds, rpt.Recommendation)
		fmt.Fprintln(os.Stderr, "aidev: Critic returned "+rpt.Recommendation+". Running Clarifier to turn its sharp questions into an interview...")

		graph, err := clarifier.Run(ctx, orch.AgentContext())
		if err != nil {
			fatal(fmt.Sprintf("clarifier: %v", err))
		}
		if len(graph.Questions) == 0 {
			fmt.Fprintln(os.Stderr, "aidev: Clarifier produced no questions — nothing unambiguous to ask about. Halting with the current verdict.")
			return rpt
		}

		waves := agents.BatchByDependencies(graph)
		answers := make([]agents.Answer, 0, len(graph.Questions))
		fmt.Fprintf(os.Stderr, "\n%d question(s) in %d wave(s). Answer each, then press enter. Leave blank to skip.\n\n",
			len(graph.Questions), len(waves))
		for wi, wave := range waves {
			fmt.Fprintf(os.Stderr, "— Wave %d (%d question(s)) —\n\n", wi+1, len(wave))
			for _, q := range wave {
				fmt.Fprintf(os.Stderr, "%s: %s\n> ", q.ID, q.Text)
				line, readErr := reader.ReadString('\n')
				if readErr != nil && line == "" {
					fatal(fmt.Sprintf("read answer: %v", readErr))
				}
				answers = append(answers, agents.Answer{
					ID:   q.ID,
					Text: strings.TrimRight(line, "\n"),
				})
			}
			fmt.Fprintln(os.Stderr)
		}

		path, err := agents.WriteClarifierMarkdown(absRepo, graph, answers)
		if err != nil {
			fatal(fmt.Sprintf("write clarifier: %v", err))
		}
		fmt.Fprintf(os.Stderr, "aidev: wrote clarifier session to %s\n", path)

		orch.AgentContext().ClarifierNotes = formatClarifierNotes(graph, answers)

		fmt.Fprintln(os.Stderr, "aidev: re-running Critic with clarifier answers...")
		for ev := range orch.Recritique(ctx) {
			if ev.Err != nil {
				fatal(ev.Err.Error())
			}
		}
		rpt = orch.CriticReport()
		if rpt == nil {
			fmt.Fprintln(os.Stderr, "aidev: Recritique returned no report; halting.")
			return nil
		}

		fmt.Println()
		fmt.Printf("## Critic report (round %d, after clarifier)\n\n", round)
		fmt.Println(rpt.Markdown)
		fmt.Println()
		fmt.Printf("RECOMMENDATION: %s\n", rpt.Recommendation)
	}

	return rpt
}

// formatClarifierNotes renders a Clarifier session (graph + answers)
// as compact markdown suitable for embedding in the Critic prompt.
// Skips unanswered questions so the Critic doesn't mistake blanks for
// negative signal. Walks in dependency wave order so the prose reads
// top-down the same way the user typed it.
func formatClarifierNotes(g *agents.QuestionGraph, answers []agents.Answer) string {
	if g == nil || len(g.Questions) == 0 {
		return ""
	}
	byID := make(map[string]string, len(answers))
	for _, a := range answers {
		byID[a.ID] = strings.TrimSpace(a.Text)
	}
	var b strings.Builder
	waves := agents.BatchByDependencies(g)
	for _, wave := range waves {
		for _, q := range wave {
			ans := byID[q.ID]
			if ans == "" {
				continue
			}
			fmt.Fprintf(&b, "- **Q (%s):** %s\n", q.ID, q.Text)
			fmt.Fprintf(&b, "  **A:** %s\n", ans)
		}
	}
	return b.String()
}

func runCompletionSubcommandWithArgs(args []string) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Println("aidev completion - Install shell autocompletion")
		fmt.Println()
		fmt.Println("Usage:")
		fmt.Println("  aidev completion <shell>")
		fmt.Println()
		fmt.Println("Available shells:")
		fmt.Println("  bash    Install bash completion")
		fmt.Println("  zsh     Install zsh completion")
		fmt.Println("  install Install completions for detected shell")
		fmt.Println()
		fmt.Println("Script output mode (for eval):")
		fmt.Println("  aidev completion bash --script    # Output bash completion script")
		fmt.Println("  aidev completion zsh --script     # Output zsh completion script")
		fmt.Println()
		fmt.Println("Examples:")
		fmt.Println("  aidev completion bash     # Install bash completion")
		fmt.Println("  aidev completion zsh      # Install zsh completion")
		fmt.Println("  aidev completion install  # Auto-detect and install")
		fmt.Println()
		fmt.Println("After installation, restart your shell or source the configuration file.")
		return
	}

	shell := args[0]

	// Check for script output mode
	if len(args) > 1 && args[1] == "--script" {
		switch shell {
		case "bash":
			fmt.Print(completion.GenerateBashCompletion())
		case "zsh":
			fmt.Print(completion.GenerateZshCompletion())
		default:
			fatal(fmt.Sprintf("unsupported shell for script output: %s", shell))
		}
		return
	}

	installer, err := completion.NewInstaller()
	if err != nil {
		fatal(fmt.Sprintf("completion installer: %v", err))
	}

	switch shell {
	case "bash":
		if err := installer.InstallBashCompletion(); err != nil {
			fatal(fmt.Sprintf("bash completion install: %v", err))
		}
		fmt.Println("Bash completion installed successfully!")
		fmt.Println("Restart your shell or run: source ~/.bashrc")
	case "zsh":
		if err := installer.InstallZshCompletion(); err != nil {
			fatal(fmt.Sprintf("zsh completion install: %v", err))
		}
		fmt.Println("Zsh completion installed successfully!")
		fmt.Println("Restart your shell or run: source ~/.zshrc")
	case "install":
		if err := installer.InstallAll(); err != nil {
			fatal(fmt.Sprintf("completion install: %v", err))
		}
		fmt.Println("Shell completion installed successfully!")
		fmt.Println("Restart your shell for changes to take effect")
	default:
		fatal(fmt.Sprintf("unsupported shell: %s (supported: bash, zsh, install)", shell))
	}
}

func runCompletionSubcommand() {
	if len(os.Args) < 2 || os.Args[1] == "--help" || os.Args[1] == "-h" {
		fmt.Println("aidev completion - Install shell autocompletion")
		fmt.Println()
		fmt.Println("Usage:")
		fmt.Println("  aidev completion <shell>")
		fmt.Println()
		fmt.Println("Available shells:")
		fmt.Println("  bash    Install bash completion")
		fmt.Println("  zsh     Install zsh completion")
		fmt.Println("  install Install completions for detected shell")
		fmt.Println()
		fmt.Println("Examples:")
		fmt.Println("  aidev completion bash     # Install bash completion")
		fmt.Println("  aidev completion zsh      # Install zsh completion")
		fmt.Println("  aidev completion install  # Auto-detect and install")
		fmt.Println()
		fmt.Println("After installation, restart your shell or source the configuration file.")
		if len(os.Args) < 2 {
			os.Exit(1)
		}
		return
	}

	shell := os.Args[1]

	installer, err := completion.NewInstaller()
	if err != nil {
		fatal(fmt.Sprintf("completion installer: %v", err))
	}

	switch shell {
	case "bash":
		if err := installer.InstallBashCompletion(); err != nil {
			fatal(fmt.Sprintf("bash completion install: %v", err))
		}
		fmt.Println("Bash completion installed successfully!")
		fmt.Println("Restart your shell or run: source ~/.bashrc")
	case "zsh":
		if err := installer.InstallZshCompletion(); err != nil {
			fatal(fmt.Sprintf("zsh completion install: %v", err))
		}
		fmt.Println("Zsh completion installed successfully!")
		fmt.Println("Restart your shell or run: source ~/.zshrc")
	case "install":
		if err := installer.InstallAll(); err != nil {
			fatal(fmt.Sprintf("completion install: %v", err))
		}
		fmt.Println("Shell completion installed successfully!")
		fmt.Println("Restart your shell for changes to take effect")
	default:
		fatal(fmt.Sprintf("unsupported shell: %s (supported: bash, zsh, install)", shell))
	}
}

func fatal(msg string) {
	fmt.Fprintf(os.Stderr, "aidev: %s\n", msg)
	os.Exit(1)
}
