# aidev

A local, multi-agent coding assistant TUI written in Go. Feed it a GitHub issue; it runs a tiered agent pipeline — small local models for extractive work, large cloud models for deep reasoning — and hands you a defensible recommendation before a single line of code is written.

> **Status:** v0.2d — Full agent pipeline (Scout + Critic + Architect + Charter + Implementer + Tester + Reviewer) + `aidev doctor` + automatic GitHub audit trail + Claude Code CLI provider as the default backend + one-command Claude Code plugin install.

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
- **For the medium and large tiers (default):** [Claude Code](https://claude.ai/download) installed and authenticated (`claude /login`). aidev routes Critic/Architect/Charter/Implementer/Reviewer through `claude --print`, so agent calls bill against your Max/Pro subscription. No `ANTHROPIC_API_KEY` required.
- **For the small tier:** a local [Ollama](https://ollama.com) daemon with a coder model pulled (e.g. `ollama pull qwen2.5-coder:7b`). Used for the Scout and Tester. Optional — you can route these to the large tier by editing `config/models.yaml`.
- `GITHUB_TOKEN` in the environment for private issues and for the controller's audit trail (Issues: Read and write scope).
- **Alternative to Claude Code CLI:** if you prefer the direct REST API, edit `config/models.yaml` to set `provider: anthropic` on the medium/large tiers and export `ANTHROPIC_API_KEY`.

## Claude Code plugin (install once, use from inside any Claude Code session)

aidev ships a set of slash commands that integrate directly into Claude Code. Install them with one command:

```sh
aidev plugin install
```

This copies five commands into `~/.claude/commands/` (or `$CLAUDE_CONFIG_DIR/commands/` if set), without overwriting any existing files:

| Slash command | Effect |
|---|---|
| `/aidev-run <issue-url> <repo-path>` | Full headless pipeline: scout → critic → (auto) architect → (sketch 1) implementer, writes `.aidev/proposed.patch` |
| `/aidev-doctor` | Environment audit (config, keys, CLI, Ollama) |
| `/aidev-charter <repo-path>` | 5-question interview, writes `.aidev/charter.md` |
| `/aidev-test <repo-path>` | Detect and run the project's test suite |
| `/aidev-review <issue-url> <repo-path>` | Boy Scout pass on `.aidev/proposed.patch` with blockers + follow-up proposals |

Pass `--force` to overwrite existing commands. Uninstall with `aidev plugin uninstall` (locally-modified files are always preserved — the uninstaller only removes files whose content matches the shipped version).

After install, type `/` in any Claude Code session and you'll see the new commands listed alongside your existing ones.

## Build & run

```sh
go build -o aidev ./cmd/aidev

# Audit the environment before doing any real work (config, Anthropic key,
# Ollama daemon, required models). Exits 1 on any FAIL.
./aidev doctor

# TUI mode — three panes: issue, scout brief, critic report (rightmost pane
# retitles to "Architect sketches" once the Architect has produced output).
# On startup, aidev runs the doctor automatically. In an interactive terminal
# it will offer to auto-spawn Ollama and pull/swap missing models.
./aidev -issue https://github.com/owner/repo/issues/42 -repo ../owner-repo

# Override the Architect's sketch count (default 3)
./aidev -issue ... -repo ... -n 5

# Headless mode — scout + critic only, prints to stdout
./aidev -headless -issue ... -repo ...

# Headless mode with auto-architect — if Critic says "build", automatically
# run the Architect and print sketches too. Useful in CI.
./aidev -headless -auto -n 3 -issue ... -repo ...

# Bypass the startup doctor (not recommended; use when you manage the
# environment yourself and want to save a few hundred ms on cold start)
./aidev -skip-doctor -issue ... -repo ...
```

## Doctor

`aidev doctor` is a precondition audit that runs automatically on every `aidev` invocation (unless you pass `-skip-doctor`). It checks:

| Check | Fails when | Fix |
|---|---|---|
| `config` | `models.yaml` missing tiers or routing | edit `config/models.yaml` |
| `anthropic-key` | a role routes to an anthropic tier but `ANTHROPIC_API_KEY` is unset | `export ANTHROPIC_API_KEY=sk-ant-...` |
| `ollama-binary` | any role routes to ollama but the `ollama` CLI isn't on PATH | install from ollama.com |
| `ollama-daemon` | binary present but `/api/tags` unreachable | aidev will offer to run `ollama serve` in the background (interactive TTY only) |
| `ollama-models` | required models aren't pulled | aidev will offer to **pull**, **swap to an installed model**, or **abort** (interactive TTY only) |

In headless/CI mode the doctor never prompts — it reports and fails fast, so you know exactly what to fix. Run `aidev doctor` explicitly to get the full report and a zero/non-zero exit code for a CI gate.

### First-pull consent

aidev will never silently download a multi-GB model. When a required model is missing, you'll see:

```
Ollama model "qwen2.5-coder:7b" is required but not installed.

  [1] Pull qwen2.5-coder:7b now (multi-GB download, requires network)
  [2] Swap to one of your already-installed models
  [3] Abort — aidev cannot run without a model for this tier

Choice [1/2/3]:
```

Choosing `2` lists the models already pulled on your Ollama daemon and swaps the routing in-memory for the current run. To make the swap permanent, edit `config/models.yaml` to reference your chosen model.

The Ollama daemon lifecycle is **not owned by aidev** — if we spawn it, it persists after aidev exits so we don't interfere with other things that use it.

## Charter

When aidev runs against a repo that has no strong signal for the Critic to anchor against — no `README.md` (or a near-empty one), no `CLAUDE.md`, no `ARCHITECTURE.md`, no `.aidev/charter.md` — the Critic has nothing to cite when pushing back on proposed features. The **Charter** agent fixes that by interviewing you and writing a clean product charter to `.aidev/charter.md` in the target repo.

### Running the interview

```sh
aidev charter -repo /path/to/target
```

You'll be asked five questions:

1. In one sentence, what does this product do?
2. Who uses it?
3. What's the single most important constraint? (correctness / latency / cost / security / compliance / something else)
4. What is explicitly out of scope — things this product should NOT do?
5. Any engineering principles that override or extend the defaults? (optional)

Your raw answers are then synthesised into a structured Markdown charter and written to `<repo>/.aidev/charter.md`. On subsequent runs, the Scout automatically absorbs this charter alongside README/CLAUDE.md/ARCHITECTURE.md and the Critic cites it when arguing about trade-offs.

### Nudges during normal runs

If you invoke `aidev -issue ... -repo ...` against a repo with no strong signal and no existing charter, aidev prints a warning to stderr pointing at `aidev charter` before the pipeline starts. The pipeline still runs — the warning is advisory.

## Audit trail (controller)

Every significant state transition — Scout started, Critic report ready, user approved, Architect produced sketches, run killed, error — is posted to the GitHub issue that seeded the run. A single **pinned status comment** is updated in place for every transition, and **one-shot artifact comments** contain the full Scout brief, Critic report, and Architect sketches.

A single `aidev:<phase>` label on the issue gives you a kanban-style view of every run at a glance on the repo's Issues page. Labels are swapped (not stacked) so there's always exactly one `aidev:*` label on an issue.

**Agents stay pure.** The controller pattern puts all GitHub I/O in the orchestrator — agents receive a `Context`, call an LLM, return markdown. They never touch the network. This keeps unit tests deterministic and keeps the failure modes of the pipeline contained to one place.

**Idempotent across re-runs.** The pinned status comment carries a hidden HTML fingerprint (`<!-- aidev:status -->`). On startup, aidev scans existing comments for that fingerprint and updates the existing one instead of posting a duplicate — so re-running aidev on the same issue doesn't spam the thread.

**Disable with `-no-audit-trail`.** The controller is on by default when the run has a GitHub issue. Pass `-no-audit-trail` to run silently.

**Credential.** The audit trail writes require `GITHUB_TOKEN` in the environment, with Issues: read and write permission. The same token aidev already uses for issue fetching.

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

- **v0.2a** — Architect agent with N divergent sketches, `-n` flag, cost preview. *(shipped)*
- **v0.2b** — `aidev doctor` + Ollama auto-spawn + first-pull consent with alternative-model offering. *(shipped)*
- **v0.2b.1** — automatic GitHub audit trail (pinned status comment, artifact comments, phase labels) via a controller that keeps agents pure. *(shipped)*
- **v0.2c** — Charter agent + `aidev charter` subcommand + `.aidev/charter.md` + Scout + Critic integration. *(shipped)*
- **v0.3a** — Implementer agent: produces a unified git diff from a chosen sketch. *(shipped)*
- **v0.3b** — Tester agent: detects the project's test runner, executes it, and summarises failures. *(shipped)*
- **v0.4** — Reviewer agent: Boy Scout pass on a patch with blockers/suggestions/follow-up issue proposals. New `aidev review` subcommand. Full agent pipeline complete. *(shipped)*
- **v0.2d** *(this release)* — Claude Code CLI provider as the default backend for medium and large tiers (works out of the box with a Max/Pro subscription, no API key required), plus `aidev plugin install` for Claude Code slash-command integration.
- **v0.5+** — Adaptive dialogue (dependency-aware question graphs), auto-filing of follow-up issues via `aidev followups --file-issues`, sandboxing for the Tester, streaming LLM responses in the TUI, two-turn Implementer file-content loading.
- **v0.4** — Reviewer + Boy Scout pass; auto-open follow-up issues for out-of-scope improvements.
- **v0.5** — Streaming LLM responses in the TUI.

## Layout

```
cmd/aidev/              entry point, flag handling, doctor subcommand
internal/config/        YAML loader for models + principles
internal/llm/           Provider interface, Ollama + Claude backends, Router
internal/github/        REST client for fetching issues
internal/repo/          Repo scanner + repo-local principle loader
internal/agents/        Scout, Critic, Architect agents (shared Context, pure)
internal/orchestrator/  State machine + Reporter interface + GitHubReporter (controller)
internal/tui/           Bubble Tea model, update, view, keybindings
internal/doctor/        Precondition checks: config, keys, claude CLI, Ollama daemon/models
internal/plugin/        Claude Code slash command installer (embedded files)
plugin/aidev/           Slash command source (also embedded in the binary)
config/                 Shipped defaults: models.yaml, principles.yaml
```
