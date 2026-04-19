// `aidev init` subcommand (v0.5).
//
// Guided first-run onboarding. The problem: a user who installs aidev
// without Ollama present ends up with a binary that can't run any of
// the four Ollama-backed profiles. `aidev doctor` tells them what's
// missing, but the user is still left to figure out WHICH profile to
// switch to and HOW. `aidev init` closes that gap: one command,
// 2-3 prompts, land on a working config.
//
// Shape:
//   aidev init                       interactive flow
//   aidev init --profile NAME --yes  non-interactive (CI, scripted)
//   aidev init --force               overwrite existing config
//
// Interactive flow (happy path, no Ollama):
//
//   1. Prereq check: claude CLI + ollama presence + GITHUB_TOKEN
//   2. Profile pick (cloud-only recommended when Ollama missing)
//   3. Optional Ollama install offer (brew / curl|sh) if the user
//      picks a profile that requires it
//   4. Write ~/.config/aidev/models.yaml by running the shipped
//      installer + switching active_profile to the user's pick
//   5. Run aidev doctor as a hard gate. If doctor fails, the user
//      sees the failure and we exit non-zero — no "you're ready"
//      on a broken config.
//
// Non-interactive flow (--yes):
//   Same shape, but no prompts. If a profile needs a missing
//   prereq, we fail loudly instead of offering to install —
//   scripted callers shouldn't trigger side effects.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/aisu-ai/aidev/internal/config"
	"github.com/aisu-ai/aidev/internal/doctor"
	"github.com/aisu-ai/aidev/internal/installpkg"
)

// initProfileDescriptor is the user-facing menu entry for a shipped
// profile. Kept in sync by hand with config/models.yaml — if you add
// a new profile there, add it here too so the init menu surfaces it.
type initProfileDescriptor struct {
	name         string
	summary      string
	requiresOllama bool
}

// shippedInitProfiles lists the profiles `aidev init` offers, in the
// order they appear in the menu. cloud-only goes first because it's
// the safest pick for a user running init for the first time.
var shippedInitProfiles = []initProfileDescriptor{
	{
		name:           "cloud-only",
		summary:        "No Ollama needed. Every agent runs through your Claude Code subscription. Recommended when Ollama isn't installed.",
		requiresOllama: false,
	},
	{
		name:           "default",
		summary:        "Local Ollama for Scout/Tester/Implementer/Reviewer; Claude Code for Critic/Architect/Coordinator. Requires Ollama (~5GB VRAM for the 14B coder model).",
		requiresOllama: true,
	},
	{
		name:           "high-vram",
		summary:        "Bigger local models (32B Implementer, 14B Scout). Requires Ollama + ~24GB VRAM.",
		requiresOllama: true,
	},
	{
		name:           "low-vram",
		summary:        "Every Ollama tier on a 7B model. For laptops with ~5GB VRAM total. Requires Ollama.",
		requiresOllama: true,
	},
	{
		name:           "offline",
		summary:        "Fully offline. Every agent on a local Ollama model (zero cloud calls). Requires Ollama.",
		requiresOllama: true,
	},
}

// runInitSubcommand is the entrypoint dispatched from main.go for
// `aidev init`. See the package comment at the top of this file for
// the flow's shape.
func runInitSubcommand() {
	var (
		profileFlag = flag.String("profile", "", "Profile to activate (skips interactive pick). One of: cloud-only, default, high-vram, low-vram, offline")
		yesFlag     = flag.Bool("yes", false, "Non-interactive mode: assume 'yes' to confirmations, fail instead of offering to install missing prereqs")
		forceFlag   = flag.Bool("force", false, "Overwrite an existing models.yaml / principles.yaml")
		dirFlag     = flag.String("dir", "", "Target config directory (default: $XDG_CONFIG_HOME/aidev or ~/.config/aidev)")
		skipDoctor  = flag.Bool("skip-doctor", false, "Skip the final aidev doctor verification (not recommended)")
	)
	flag.Parse()

	target := *dirFlag
	if target == "" {
		resolved, err := installpkg.DefaultConfigDir()
		if err != nil {
			fatal(fmt.Sprintf("init: %v", err))
		}
		target = resolved
	}

	// Detect interactive vs. scripted. --yes forces scripted mode
	// regardless of TTY (so CI with a TTY still skips prompts).
	interactive := !*yesFlag && isTTY()

	// STEP 1 — Prereq check.
	prereqs := checkInitPrereqs()
	printInitBanner(prereqs)

	// STEP 2 — Profile pick.
	var picked string
	if *profileFlag != "" {
		if !isValidInitProfile(*profileFlag) {
			fmt.Fprintf(os.Stderr, "aidev init: unknown profile %q. Available: %s\n", *profileFlag, strings.Join(initProfileNames(), ", "))
			os.Exit(2)
		}
		picked = *profileFlag
	} else if interactive {
		picked = promptForProfile(prereqs)
	} else {
		// Non-interactive with no --profile flag — pick cloud-only as
		// the safest default. Scripted callers who want something
		// else should pass --profile explicitly.
		picked = "cloud-only"
		fmt.Fprintln(os.Stderr, "aidev init: no --profile flag and not a TTY; defaulting to cloud-only.")
	}

	// STEP 3 — If the picked profile needs Ollama and it's missing,
	// offer to install (interactive) or fail loudly (scripted).
	desc := initProfileByName(picked)
	if desc.requiresOllama && !prereqs.ollama {
		if !interactive {
			fatal(fmt.Sprintf("init: profile %q requires Ollama but `ollama` is not on PATH. Install from https://ollama.com/download, or rerun with --profile cloud-only.", picked))
		}
		if !offerOllamaInstall() {
			// User declined. Offer to downgrade to cloud-only; if
			// they decline that too, abort.
			fmt.Fprintln(os.Stderr)
			fmt.Fprintf(os.Stderr, "Profile %q won't work without Ollama.\n", picked)
			if confirmPrompt(bufio.NewReader(os.Stdin), "Use cloud-only profile instead?", true) {
				picked = "cloud-only"
			} else {
				fatal("init: aborted (Ollama missing, user declined cloud-only fallback)")
			}
		}
		// If they accepted and the install succeeded, re-check so
		// the doctor at the end sees the fresh state.
		prereqs = checkInitPrereqs()
	}

	// STEP 4 — Write config. InstallConfig lays down the shipped
	// default models.yaml (active_profile=default). If the user
	// picked something else, switch the active profile after
	// writing.
	if err := writeInitConfig(target, picked, *forceFlag); err != nil {
		fatal(fmt.Sprintf("init: %v", err))
	}
	fmt.Fprintf(os.Stderr, "\n✓ Wrote %s/models.yaml (active_profile=%s)\n", target, picked)

	// STEP 5 — Run doctor as the final gate. If it fails, the user
	// sees the exact failure and we exit non-zero.
	if *skipDoctor {
		fmt.Fprintln(os.Stderr, "\nSkipping doctor (--skip-doctor).")
		printInitNextSteps(picked)
		return
	}

	fmt.Fprintln(os.Stderr, "\nRunning aidev doctor to verify...")
	cfg, err := config.Load(target)
	if err != nil {
		fatal(fmt.Sprintf("init: load config after write: %v", err))
	}
	report := doctor.Run(context.Background(), cfg, doctor.Options{
		Interactive:     false,
		AutoSpawnOllama: false,
		Out:             os.Stderr,
	})
	report.Write(os.Stderr)
	if report.HasFailure() {
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "aidev doctor reported failures against the new config. See the output above for what to fix.")
		fmt.Fprintln(os.Stderr, "Rerun `aidev init --profile <name>` once the prereqs are in place, or `aidev doctor` to re-verify.")
		os.Exit(1)
	}

	fmt.Fprintln(os.Stderr, "\n✓ aidev doctor: all checks pass.")
	printInitNextSteps(picked)
}

// initPrereqs captures the state of the three things aidev init
// cares about: Claude Code auth, Ollama binary, GitHub token.
type initPrereqs struct {
	claudeCLI    bool
	ollama       bool
	githubToken  bool
	claudePath   string
	ollamaPath   string
}

// checkInitPrereqs runs the cheapest possible detection for each
// prereq. We don't verify Claude Code is actually authenticated
// (that's `aidev doctor`'s job); we just check the binary exists
// on PATH. Same for Ollama.
func checkInitPrereqs() initPrereqs {
	p := initPrereqs{}
	if path, err := exec.LookPath("claude"); err == nil {
		p.claudeCLI = true
		p.claudePath = path
	}
	if path, err := exec.LookPath("ollama"); err == nil {
		p.ollama = true
		p.ollamaPath = path
	}
	// GITHUB_TOKEN is considered available if it's in the env OR
	// `gh auth token` resolves to something non-empty.
	if strings.TrimSpace(os.Getenv("GITHUB_TOKEN")) != "" {
		p.githubToken = true
	} else if out, err := exec.Command("gh", "auth", "token").Output(); err == nil && len(strings.TrimSpace(string(out))) > 0 {
		p.githubToken = true
	}
	return p
}

// printInitBanner renders the welcome header + prereq check. This
// always prints, even in non-interactive mode, because the log is
// useful when `aidev init --yes` fails.
func printInitBanner(p initPrereqs) {
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "Welcome to aidev. Let's get you set up.")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "[1/3] Checking prerequisites...")
	mark := func(ok bool) string {
		if ok {
			return "  ✓"
		}
		return "  ✗"
	}
	fmt.Fprintf(os.Stderr, "%s Claude Code (claude CLI)\n", mark(p.claudeCLI))
	fmt.Fprintf(os.Stderr, "%s Ollama\n", mark(p.ollama))
	fmt.Fprintf(os.Stderr, "%s GITHUB_TOKEN resolvable\n", mark(p.githubToken))
	fmt.Fprintln(os.Stderr)
	if !p.claudeCLI {
		fmt.Fprintln(os.Stderr, "    claude CLI is required for every shipped profile. Install it from https://claude.ai/download and run `claude /login`, then rerun `aidev init`.")
	}
	if !p.githubToken {
		fmt.Fprintln(os.Stderr, "    GITHUB_TOKEN is not set and `gh auth token` didn't resolve one. aidev needs a GitHub token to read/comment on issues.")
		fmt.Fprintln(os.Stderr, "    Install the gh CLI and run `gh auth login`, or export GITHUB_TOKEN=<pat>, then rerun `aidev init`.")
	}
}

// promptForProfile shows the shippedInitProfiles menu and returns
// the name the user picked. Recommends cloud-only when Ollama is
// missing, otherwise default.
func promptForProfile(p initPrereqs) string {
	fmt.Fprintln(os.Stderr, "[2/3] Pick a profile:")
	fmt.Fprintln(os.Stderr)
	recommended := "cloud-only"
	if p.ollama {
		recommended = "default"
	}
	for i, d := range shippedInitProfiles {
		letter := string(rune('a' + i))
		marker := "   "
		if d.name == recommended {
			marker = " ★ "
		}
		fmt.Fprintf(os.Stderr, "%s(%s) %-12s %s\n", marker, letter, d.name, d.summary)
	}
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "★ = recommended for your setup")
	fmt.Fprintln(os.Stderr)

	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Fprintf(os.Stderr, "Pick [a-%s, default=%s]: ", string(rune('a'+len(shippedInitProfiles)-1)), letterForProfile(recommended))
		line, err := reader.ReadString('\n')
		if err != nil {
			fatal(fmt.Sprintf("init: read profile pick: %v", err))
		}
		choice := strings.TrimSpace(line)
		if choice == "" {
			return recommended
		}
		// Accept either the letter or the name itself.
		if len(choice) == 1 {
			idx := int(choice[0] - 'a')
			if idx >= 0 && idx < len(shippedInitProfiles) {
				return shippedInitProfiles[idx].name
			}
		}
		if isValidInitProfile(choice) {
			return choice
		}
		fmt.Fprintf(os.Stderr, "  unknown choice %q. Pick a letter (a-%s) or a profile name.\n", choice, string(rune('a'+len(shippedInitProfiles)-1)))
	}
}

// offerOllamaInstall offers to install Ollama via the appropriate
// package manager. Returns true if the install succeeded and Ollama
// is now on PATH; false if the user declined or the install failed.
func offerOllamaInstall() bool {
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "This profile needs Ollama, and it isn't installed.")
	var (
		cmd         *exec.Cmd
		description string
	)
	switch runtime.GOOS {
	case "linux":
		description = "curl -fsSL https://ollama.com/install.sh | sh"
		cmd = exec.Command("sh", "-c", "curl -fsSL https://ollama.com/install.sh | sh")
	case "darwin":
		if _, err := exec.LookPath("brew"); err == nil {
			description = "brew install ollama"
			cmd = exec.Command("brew", "install", "ollama")
		} else {
			fmt.Fprintln(os.Stderr, "  Homebrew not detected. Download the macOS installer from https://ollama.com/download, then rerun `aidev init`.")
			return false
		}
	default:
		fmt.Fprintf(os.Stderr, "  Platform %s has no automatic installer here. Install from https://ollama.com/download, then rerun `aidev init`.\n", runtime.GOOS)
		return false
	}

	fmt.Fprintf(os.Stderr, "  Installer:  %s\n", description)
	if !confirmPrompt(bufio.NewReader(os.Stdin), "Run this now?", true) {
		return false
	}
	fmt.Fprintln(os.Stderr)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "  install failed: %v\n", err)
		return false
	}
	// Re-check that `ollama` now resolves on PATH. `brew install
	// ollama` puts it into a well-known prefix that the user's shell
	// already has on PATH; `curl|sh` drops it into /usr/local/bin.
	if _, err := exec.LookPath("ollama"); err != nil {
		fmt.Fprintln(os.Stderr, "  installer exited 0 but `ollama` is still not on PATH. Open a new shell and rerun `aidev init`.")
		return false
	}
	fmt.Fprintln(os.Stderr, "  ✓ Ollama installed.")
	return true
}

// writeInitConfig lays down the shipped default config via
// InstallConfig, then switches the active profile to the user's
// pick (unless they picked the shipped default, in which case we
// don't need to rewrite).
//
// If force is false and the target dir already contains models.yaml,
// InstallConfig will skip the file. We catch that case explicitly
// and refuse to clobber without --force, so the user's hand-edited
// customizations don't silently vanish.
func writeInitConfig(targetDir, profile string, force bool) error {
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	existing := filepath.Join(targetDir, "models.yaml")
	if _, err := os.Stat(existing); err == nil && !force {
		return fmt.Errorf("%s already exists. Pass --force to overwrite (this clobbers any hand-edits), or run `aidev config profile use %s` to switch profiles without rewriting the file", existing, profile)
	}

	if _, _, err := installpkg.InstallConfig(targetDir, true); err != nil {
		return fmt.Errorf("install shipped config: %w", err)
	}

	// InstallConfig wrote models.yaml with whatever active_profile
	// the shipped file names. Load it and switch if needed.
	cfg, err := config.Load(targetDir)
	if err != nil {
		return fmt.Errorf("reload config after install: %w", err)
	}
	if cfg.ActiveProfile == profile {
		return nil
	}
	if err := writeActiveProfile(targetDir, profile); err != nil {
		return fmt.Errorf("switch active profile to %q: %w", profile, err)
	}
	return nil
}

// printInitNextSteps is the final banner shown on a successful init.
// Mirrors the tone of install.sh's closing block.
func printInitNextSteps(profile string) {
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "================================================================================")
	fmt.Fprintf(os.Stderr, "  You're ready (profile: %s).\n", profile)
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "  Try a real run:")
	fmt.Fprintln(os.Stderr, "    aidev -issue <number-or-url> -repo <path>")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "  Or from a Claude Code session:")
	fmt.Fprintln(os.Stderr, "    /aidev-run <issue> <repo>")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "  Switch profiles later:")
	fmt.Fprintln(os.Stderr, "    aidev config profile list")
	fmt.Fprintln(os.Stderr, "    aidev config profile use <name>")
	fmt.Fprintln(os.Stderr, "================================================================================")
}

// isValidInitProfile reports whether name is one of the shipped
// profiles aidev init knows how to activate.
func isValidInitProfile(name string) bool {
	for _, d := range shippedInitProfiles {
		if d.name == name {
			return true
		}
	}
	return false
}

// initProfileByName returns the descriptor for a known profile,
// or a zero-value descriptor if the name doesn't match. Callers
// should guard with isValidInitProfile first.
func initProfileByName(name string) initProfileDescriptor {
	for _, d := range shippedInitProfiles {
		if d.name == name {
			return d
		}
	}
	return initProfileDescriptor{}
}

// initProfileNames returns the shipped profile names in a stable
// order — menu order, not alphabetical — so error messages match
// the menu the user just saw.
func initProfileNames() []string {
	names := make([]string, 0, len(shippedInitProfiles))
	for _, d := range shippedInitProfiles {
		names = append(names, d.name)
	}
	// Sort only when we're generating a free-standing error message
	// (no visible menu to compare against).
	sort.Strings(names)
	return names
}

// letterForProfile maps a profile name to its menu letter (a/b/c/d/e).
func letterForProfile(name string) string {
	for i, d := range shippedInitProfiles {
		if d.name == name {
			return string(rune('a' + i))
		}
	}
	return "a"
}

// confirmPrompt reads a y/n answer from r. Returns defaultYes when
// the user just hits Enter.
func confirmPrompt(r *bufio.Reader, question string, defaultYes bool) bool {
	hint := "[Y/n]"
	if !defaultYes {
		hint = "[y/N]"
	}
	fmt.Fprintf(os.Stderr, "%s %s ", question, hint)
	line, err := r.ReadString('\n')
	if err != nil {
		return defaultYes
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	if answer == "" {
		return defaultYes
	}
	return answer == "y" || answer == "yes"
}

// isTTY reports whether stdin looks interactive. Reuses doctor's
// helper so the rules match across subcommands (doctor and init
// share the "auto-detect TTY, let --yes force scripted mode"
// convention).
func isTTY() bool {
	return doctor.IsTTY()
}
