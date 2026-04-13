# aidev

A local, multi-agent coding assistant TUI written in Go. Feed it a GitHub issue; it runs a tiered agent pipeline — small local models for extractive work, large cloud models for deep reasoning — and hands you a defensible recommendation before a single line of code is written.

> **Status:** v0.2a — Scout + Critic + Architect. Implementer, Tester, Reviewer are on the roadmap.

## Why

Several good tools already turn issues into patches (aider, plandex, opencode, goose, Claude Code). None of them *argue with you* before implementation. aidev's distinguishing feature is the **Critic**: an adversarial agent whose single job is to answer "should this be built?" using your repository's stated purpose and your own engineering principles as the yardstick. When the answer is "yes", the **Architect** produces N divergent solution sketches so you pick the *approach* before any code is written.

## Architecture

```
┌─────────────────────────────────────────────┐
│ Orchestrator (state machine)                │
└──┬──────┬──────────┬──────┬──────┬──────────┘
   │      │          │      │      │
 Scout  Critic  Architect  Impl  Tester   (v0.2a ships the first three)
```

- **Scout** — reads the target repo (README, CLAUDE.md, ARCHITECTURE.md, file layout) and produces a factual brief. Runs on the **small local tier** (Ollama).
- **Critic** — takes the brief + the issue + your principles and produces a `build | defer | kill | unclear` recommendation with arguments for and against. Runs on the **large tier** (Claude).
- **Architect** — after the human approves the Critic's "build" verdict, produces N *meaningfully different* solution sketches with trade-offs, risks, principle alignment, and a rough scope estimate for each. Runs on the **large tier**. Default N is 3; override with `-n`.
- **Implementer / Tester / Reviewer** — next milestone.

Model routing is declarative (`config/models.yaml`): each role is mapped to a named tier, and each tier is mapped to a provider. Swap tiers freely.

## Requirements

- Go 1.24+
- For the small tier: a local [Ollama](https://ollama.com) daemon with a coder model pulled (e.g. `ollama pull qwen2.5-coder:7b`).
- For the large tier: `ANTHROPIC_API_KEY` in the environment.
- `GITHUB_TOKEN` in the environment for private issues (public issues work without it, but rate limits are tighter).

## Build & run

```sh
go build -o aidev ./cmd/aidev

# TUI mode — three panes: issue, scout brief, critic report (rightmost pane
# retitles to "Architect sketches" once the Architect has produced output).
./aidev -issue https://github.com/owner/repo/issues/42 -repo ../owner-repo

# Override the Architect's sketch count (default 3)
./aidev -issue ... -repo ... -n 5

# Headless mode — scout + critic only, prints to stdout
./aidev -headless -issue ... -repo ...

# Headless mode with auto-architect — if Critic says "build", automatically
# run the Architect and print sketches too. Useful in CI.
./aidev -headless -auto -n 3 -issue ... -repo ...
```

## TUI keybindings

| Key | Action |
|-----|--------|
| `r` | Run Scout + Critic |
| `a` | After Critic: approve → kick off Architect |
| `k` | Kill the proposal |
| `tab` | Cycle pane focus |
| `q` / `ctrl+c` | Quit |

The status bar shows a **cost preview** for the Architect run (sketch count, estimated output tokens, estimated seconds) before you approve, so the bill is never a surprise.

## Configuration

### `config/models.yaml`

Defines three tiers (small / medium / large) and a `routing` table that maps agent roles to tiers. Edit this file to point tiers at different models or providers. The Architect is routed to the `large` tier by default because sketch divergence needs deep reasoning.

### `config/principles.yaml`

The living charter the Critic uses. Starts with "Should this exist?", Boy Scout Rule, DRY, YAGNI, Well-Architected, Single Responsibility, Fail Loudly at Boundaries, Tests Describe Intent, and Reversibility. Add your own.

A target repository can ship its own `.aidev/principles.yaml` which is merged on top of the global set — useful when an organisation has a standards repo (like `ai-dev-standards`) that downstream projects inherit from.

## Sketch format

The Architect is required to emit sketches in a parseable structure:

```
## Sketch 1: <short title>

### Approach
...

### Key decisions
- ...

### Trade-offs
- pro: ...
- con: ...

### Risks
- ...

### Principle alignment
- <Principle Name>: aligned / tension / violation — why

### Rough scope
Files touched, LOC estimate, migrations, new dependencies.

---

## Sketch 2: ...
```

Sketches are Markdown only — no code. The Implementer (v0.3) is what writes code against a chosen sketch.

## Roadmap

- **v0.2a** *(this release)* — Architect agent with N divergent sketches, `-n` flag, cost preview.
- **v0.2b** — `aidev doctor` + Ollama auto-spawn + first-pull consent.
- **v0.2c** — Charter agent + `.aidev/charter.md` + interview flow for repos without a clear stated purpose.
- **v0.3** — Implementer + Tester in an isolated git worktree + container, with full-coverage tests gating completion. Adaptive dialogue (dependency-aware question graphs).
- **v0.4** — Reviewer + Boy Scout pass; auto-open follow-up issues for out-of-scope improvements.
- **v0.5** — Streaming LLM responses in the TUI.

## Layout

```
cmd/aidev/              entry point and flag handling
internal/config/        YAML loader for models + principles
internal/llm/           Provider interface, Ollama + Claude backends, Router
internal/github/        REST client for fetching issues
internal/repo/          Repo scanner + repo-local principle loader
internal/agents/        Scout, Critic, Architect agents (shared Context)
internal/orchestrator/  State machine that drives the pipeline
internal/tui/           Bubble Tea model, update, view, keybindings
config/                 Shipped defaults: models.yaml, principles.yaml
```
