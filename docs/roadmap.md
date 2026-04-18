---
title: Roadmap
---

# Roadmap

## Shipped

### v0.3.0 (initial public release, 2026-04-18)

- **P0 Selector** — autonomous sketch picking via deterministic rubric + LLM tie-breaker
- **P1 runDir** — generated artifacts relocated out of the target repo into `$XDG_DATA_HOME/aidev/runs/`
- **P2 GH audit trail** — every decision point posts as a first-class comment on the issue
- **P3 Autonomous `/aidev-run` flow** — `-interactive` gated, default is headless
- **P4 Auto-PR** — slash command opens PR against `preview` with safety guards (dirty-tree check, branch collision, test-run before push)
- **P5 Needs-refinement state** — structured escalation when the Critic can't reach a verdict
- **P6 Autonomous review → fix loop** — Triage agent + Sketch-grounded rebuttal safety + security-finding lock + protected-path forced escalation + cycle protection
- **Deterministic safety hooks** — pre-push tests/audit, post-PR-create state persistence, Stop-blocks-on-red-CI
- **Branch-first flow** — `gh issue develop` cuts the feature branch from `origin/<base>` before any implementation edit
- **Open source launch** — MIT license, public repo, `docs/` site, branch protection on main with required `build-test` CI

### v0.2 series (private development)

- **v0.2e** — one-command `install.sh`, XDG config discovery, prebuilt release binaries
- **v0.2d** — Claude Code CLI provider, `aidev plugin install`
- **v0.2c** — Charter agent + interview subcommand
- **v0.2b.1** — Controller for automatic GitHub issue audit trail
- **v0.2b** — `aidev doctor`, Ollama auto-spawn, first-pull consent
- **v0.2a** — Architect with N divergent sketches, cost preview
- **v0.2f (transitional)** — 5-gate Coordinator monitor, native tool use in `llm.Provider`, profile-based config, Ollama tool use

### Pre-v0.2

- **v0.1** — Scout + Critic + Bubble Tea TUI (the keystone — the Critic argues before writing code)

## Not yet

### Near-term (filed as GitHub issues, PRs welcome)

- **Repo path as URL** — accept `https://github.com/owner/repo` / `git@...` / `owner/repo` shorthand and auto-clone to `$XDG_DATA_HOME/aidev/clones/`. Today `-repo` requires a local filesystem path.
- **`-headless -auto` + Critic `unclear` → structured needs-refinement comment** — currently that path posts only the Critic report + `aidev:awaiting-decision` label, not the 🤔 checklist comment P5 designed.
- **CI-driven autonomous Critic** — `issues.opened` GitHub Action that runs Scout + Critic and posts the verdict as a comment. See the [GH Actions integration issue](https://github.com/AiSU-AI/aidev/issues) for the hybrid implementation plan.
- **Homebrew formula** — `brew install aidev` for macOS one-liner install.
- **`aidev update` self-updater** — `aidev update` checks for the latest release and reinstalls.
- **SHA256 verification of downloaded release tarballs** — install.sh should verify `<asset>.sha256` alongside the download.

### Medium-term (good-first-issue candidates)

- **Windows support** — TUI currently has POSIX-isms; Windows users would need WSL today.
- **Streaming TUI progress views** — live-stream claude-cli / Ollama output instead of batch-render.
- **Homebrew tap** — `homebrew-aidev` with automated updates on release.
- **Docs site upgrade** — consider mkdocs-material if traffic justifies it; currently served via plain Jekyll on GitHub Pages.

### Long-term (design space)

- **PR-comment review-reading loop** — aidev reacts to human review comments on PRs it opened, beyond just CI checks.
- **Cross-PR context** — Selector evaluates how this issue interacts with other in-flight branches.
- **Self-tuning rubric** — learn rubric weights from "implementations that landed cleanly" vs "got reverted" in the GH issue history.
- **Enterprise GHE support** — `gh issue view` assumes github.com; needs `GITHUB_HOST` threading.

## Out of scope (deliberate)

These have been considered and deferred indefinitely:

- **Auto-merge after convergence** — even when the review loop converges to `approve`, the human merges manually. The autonomy stops at "PR ready for human review."
- **Claude Code API replacement** — aidev uses `claude-cli` as a subprocess; it doesn't try to reimplement Claude Code's tool harness in the Anthropic Messages API.
- **Proprietary plugin marketplace** — slash commands ship alongside the binary, no separate package registry.
- **Windows GUI** — the terminal / slash command surface is the product; no planned GUI client.

## See also

- [Architecture](architecture.md) — the extension points for adding new agents
- [Review loop](review-loop.md) — what P6 actually does
