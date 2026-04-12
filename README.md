# aidev

A local, multi-agent coding assistant TUI written in Go. Feed it a GitHub issue; it runs a tiered agent pipeline — small local models for extractive work, large cloud models for deep reasoning — and hands you a defensible recommendation before a single line of code is written.

> **Status:** v0.1 — Scout + Critic only. Architect, Implementer, Tester, Reviewer are on the roadmap.

## Why

Several good tools already turn issues into patches (aider, plandex, opencode, goose, Claude Code). None of them *argue with you* before implementation. aidev's distinguishing feature is the **Critic**: an adversarial agent whose single job is to answer "should this be built?" using your repository's stated purpose and your own engineering principles as the yardstick.

## Architecture

```
┌─────────────────────────────────────────────┐
│ Orchestrator (state machine)                │
└──┬──────┬──────┬──────┬──────┬──────────────┘
   │      │      │      │      │
 Scout  Critic  Architect Impl  Tester   (v0.1 ships the first two)
```

- **Scout** — reads the target repo (README, CLAUDE.md, ARCHITECTURE.md, file layout) and produces a factual brief. Runs on the **small local tier** (Ollama).
- **Critic** — takes the brief + the issue + your principles and produces a `build | defer | kill | unclear` recommendation with arguments for and against. Runs on the **large tier** (Claude).
- **Architect / Implementer / Tester / Reviewer** — next milestone.

Model routing is declarative (`config/models.yaml`): each role is mapped to a named tier, and each tier is mapped to a provider. Swap tiers freely.

## Requirements

- Go 1.24+
- For the small tier: a local [Ollama](https://ollama.com) daemon with a coder model pulled (e.g. `ollama pull qwen2.5-coder:7b`).
- For the large tier: `ANTHROPIC_API_KEY` in the environment.
- `GITHUB_TOKEN` in the environment for private issues (public issues work without it, but rate limits are tighter).

## Build & run

```sh
go build -o aidev ./cmd/aidev

# TUI mode — three panes: issue, scout brief, critic report
./aidev -issue https://github.com/owner/repo/issues/42 -repo ../owner-repo

# Headless mode — prints a single Markdown report to stdout
./aidev -issue https://github.com/owner/repo/issues/42 -repo ../owner-repo -headless
```

## TUI keybindings

| Key | Action |
|-----|--------|
| `r` | Run Scout + Critic |
| `tab` | Cycle pane focus |
| `a` | Approve Critic recommendation (build) |
| `k` | Kill the proposal |
| `q` / `ctrl+c` | Quit |

## Configuration

### `config/models.yaml`

Defines three tiers (small / medium / large) and a `routing` table that maps agent roles to tiers. Edit this file to point tiers at different models or providers.

### `config/principles.yaml`

The living charter the Critic uses. Starts with "Should this exist?", Boy Scout Rule, DRY, YAGNI, Well-Architected, Single Responsibility, Fail Loudly at Boundaries, Tests Describe Intent, and Reversibility. Add your own.

A target repository can ship its own `.aidev/principles.yaml` which is merged on top of the global set — useful when an organisation has a standards repo (like `ai-dev-standards`) that downstream projects inherit from.

## Roadmap

- **v0.2** — Architect agent: produce N solution sketches with trade-offs before the Implementer commits to one.
- **v0.3** — Implementer + Tester in an isolated git worktree + container, with full-coverage tests gating completion.
- **v0.4** — Reviewer + Boy Scout pass; auto-open follow-up issues for out-of-scope improvements.
- **v0.5** — Streaming LLM responses in the TUI.

## Layout

```
cmd/aidev/          entry point and flag handling
internal/config/    YAML loader for models + principles
internal/llm/       Provider interface, Ollama + Claude backends, Router
internal/github/    REST client for fetching issues
internal/repo/      Repo scanner + repo-local principle loader
internal/agents/    Scout and Critic agents (shared Context)
internal/orchestrator/  State machine that drives the pipeline
internal/tui/       Bubble Tea model, update, view, keybindings
config/             Shipped defaults: models.yaml, principles.yaml
```
