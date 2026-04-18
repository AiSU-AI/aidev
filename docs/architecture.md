---
title: Architecture
---

# Architecture

A one-page tour of what aidev actually does under the hood, and why it's structured the way it is. Complements the [README](../README.md) (value prop + install + first-run) with the internal shape.

## The big picture

aidev is a pipeline of specialised agents wired behind one CLI entry point. The pipeline turns a GitHub issue into an architectural decision and hands implementation off to Claude Code. Every gate is deliberate; every decision lands in the GitHub issue thread so the whole chain is auditable six months later.

```
GitHub issue
    │
    ▼
┌─────────┐   ┌─────────┐   ┌──────────┐   ┌──────────┐   ┌────────┐
│  Scout  │──▶│  Critic │──▶│ Architect│──▶│ Selector │──▶│ Triage │──▶ handoff to Claude Code
└─────────┘   └─────────┘   └──────────┘   └──────────┘   └────────┘              │
    │             │              │               │             │                  │
    └── local ────┘              └── cloud-tier judgment ──────┘                  │
                                                                                  │
                                                                                  ▼
                                                              native Edit/Write/Bash implementation
                                                                  + PR + auto-review loop
```

## The agents

Each agent does one thing. Agents share a tiny contract (`Run(ctx, sharedContext) → typedOutput, error`) but never peek at each other's internal state — the shared `agents.Context` is the only boundary.

| Agent | Purpose | Typical model tier | Can stop the pipeline? |
|---|---|---|---|
| **Scout** | Read the target repo; produce a factual brief (what's here, what's not, what the stated purpose is). | Small local (Ollama 7B) | No — it just describes |
| **Critic** | Argue *against* the proposed issue using the repo's principles. Returns `build` / `defer` / `kill` / `unclear`. | Large cloud (claude-cli) | Yes — `kill` ends the pipeline, `unclear` triggers Clarifier |
| **Clarifier** | Ask the user sharp questions when the Critic's verdict is `unclear`; feed answers back for a second Critic pass. | — (interactive) | No — it escalates |
| **Architect** | Produce N structurally-different sketches of how to solve the issue. Each sketch has Approach, Key decisions, Trade-offs, Risks, Principle alignment, Rough scope. | Large cloud (claude-cli), N independent calls | No |
| **Selector** | Pick ONE sketch autonomously using a deterministic rubric (weighted scoring on principle alignment, tensions, violations, risk) with an LLM tie-breaker when scores are within epsilon. | Cloud (claude-cli) | Yes — refuses to pick if the best sketch scores below `min_implementable_score` |
| **Triage** | After the Reviewer produces findings (and CI reports status), judge each finding: `fix_now`, `rebut` (with literal Sketch citation), `defer_to_followup`, or `escalate`. Go-side coercions enforce the safety envelope. | Cloud (claude-cli) | Yes — any `escalate` converges the review loop to "needs human" |
| **Coordinator** | Runs advisory monitors at 5 gates (post-scout, post-critic, post-architect, post-implementer, post-reviewer) and surfaces `info`/`warn`/`concern` notes without blocking. | Cloud (claude-cli) | No — advisory only |
| **Implementer** / **Reviewer** / **Tester** | Lower-tier agents used by the standalone `aidev review` / `aidev test` subcommands. The main `/aidev-run` flow hands implementation off to Claude Code instead. | Local (Ollama 14B) | — |

## The two load-bearing insights

### 1. Decision belongs to aidev; implementation belongs to Claude Code.

aidev's pipeline ends at "the Selector chose Sketch N because X." The slash command hands off to Claude Code (running in the user's terminal) to do the actual file edits, tests, commits, and PR. This split is deliberate:

- **aidev is a Go binary without tool-use capability** for arbitrary file writing — it can read the repo, call LLMs, call `gh`, but it does not own the user's editor or their disk. Claude Code does.
- **Claude Code has native Edit / Write / Bash tools** wired into its agent harness. Pushing implementation there means the user keeps full visibility into every file change, every test run, every commit.
- **The audit trail survives compaction.** Claude Code's conversation may compact or end; aidev's decisions live on the GitHub issue as first-class comments, plus on disk under `$XDG_DATA_HOME/aidev/runs/<owner>-<repo>-<issue>/architect-output.md`.

### 2. Rebuttals must be grounded in literal Sketch citations.

The Triage agent can decide "the Reviewer is wrong about this finding" — but only when it can quote the chosen Sketch's markdown verbatim in a `sketch_citation` field. A rebuttal without a citation is automatically coerced to `escalate` by Go-side logic, regardless of how confident the LLM sounded. Two additional coercions complete the safety envelope:

- **Security-tool CI failures** (CodeQL, Snyk, Dependabot, Trivy, Semgrep, npm/pnpm audit) can never be rebutted — only `fix_now` or `escalate`.
- **Fixes that touch protected paths** (migrations, CI workflows, secrets, env files) are forced to `escalate`.
- **Cycle protection**: the same finding ID appearing twice across review rounds is forced to `escalate`.

This is the difference between "an LLM that argues its way to doing whatever it wants" and "an LLM operating inside a safety envelope that humans can read and trust."

## The autonomous review → fix loop (P6)

After Claude Code opens the PR, the slash command enters STEPS 12–15 of `/aidev-run`:

1. **Wait for CI** to settle on the pushed branch (`gh pr checks --watch --required --interval 30`).
2. **Capture CI status + failure logs** into `<runDir>/ci-status-round-<R>.json`.
3. **Invoke the Triage agent** via `aidev review -triage -round <R>` — this is the mandatory first action of each round; the slash command NEVER improvises from raw CI logs.
4. **Act on `convergence`**:
   - `approve` → post convergence comment, mark PR ready, done.
   - `rebut_to_ship` → post each rebuttal (with its Sketch citation) as a PR comment, mark ready, done.
   - `continue` → apply `fix_now` actions natively, run tests, commit, push, re-enter round R+1.
   - `escalate` → add `aidev:needs-human-review` label, leave PR draft, done.
5. Cap at `review.max_rounds` (default 3). After the cap, force `escalate`.

Hooks at `~/.claude/hooks/aidev-{pre-push,pr-created,stop-check}.sh` provide a deterministic backstop outside the LLM's control: pre-push blocks if `pnpm verify` / `pnpm audit` fails locally; Stop blocks if any PR created this session has failing CI without the escalation label.

## Providers and the tier system

aidev routes each agent role (`scout`, `critic`, `architect`, `selector`, `triage`, `coordinator`, `implementer`, `reviewer`, `tester`) to a named tier in `config/models.yaml`. Tiers are backed by one of three provider types:

- **`ollama`** — local models (default for `scout`, `tester`, `implementer`, `reviewer` — small + medium). Zero cost, fully offline capable.
- **`claude-cli`** — subprocess invocation of the user's `claude` binary (default for `critic`, `architect`, `selector`, `triage`, `coordinator`). Uses the user's Claude Code subscription; no API key needed.
- **`anthropic`** — direct REST calls to the Anthropic Messages API. Requires `ANTHROPIC_API_KEY`. Used only when the user explicitly swaps a tier.

The `claude-cli` subprocess mode is the differentiating design choice: it lets aidev leverage an already-authenticated Claude Code session without asking the user to manage API keys or incur a second subscription. See [`internal/llm/claude_cli.go`](../internal/llm/claude_cli.go) for the implementation.

## Per-run artifacts and the `runDir`

Every aidev run produces a directory at `$XDG_DATA_HOME/aidev/runs/<owner>-<repo>-<issue>/` (or `~/.local/share/aidev/runs/...` when XDG is unset). This directory holds:

- `architect-output.md` — full handoff doc: Selector verdict at top, then Scout brief, Critic report, all N Architect sketches.
- `clarifier.md` — if the Clarifier interview ran, the sharp questions + answers.
- `followups.md` — if the standalone Reviewer proposed follow-up issues, they live here.
- `ci-status-round-<R>.json`, `prev-actions.json` — review-loop state.

Artifacts **never** land in the target repo's working tree. This was a deliberate choice in P1 — keeping aidev artifacts out of `<repo>/.aidev/` means `git status` stays clean in the target project.

## Extending aidev

If you want to add a new agent (or swap an existing one's implementation):

1. Add a new `RoleX` to `internal/llm/provider.go` (and a fallback chain in `router.go` if it should degrade gracefully).
2. Create `internal/agents/x.go` with a `type X struct { Provider llm.Provider }` and a `Run(ctx, *Context) (*XOutput, error)` method. The contract is by convention, not an interface — every agent looks similar enough that an interface would constrain more than it helps.
3. Wire it into `internal/orchestrator/orchestrator.go`: construct in `New()`, call in `Continue()` (or the subcommand entry point for standalone agents), emit the appropriate `State*` events.
4. Extend `internal/orchestrator/github_reporter.go` if the new agent should post its own comment to the issue.
5. Update the slash command at `plugin/aidev/commands/aidev-run.md` if the agent should appear in the main autonomous flow.

See [`internal/agents/triage.go`](../internal/agents/triage.go) for a well-worked example — it exercises the full pattern including Go-side safety coercions that override LLM output.
