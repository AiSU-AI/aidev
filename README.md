# aidev

> A local multi-agent coding assistant that **argues with you before it writes code**. Built in Go. Works with your Claude Max/Pro subscription — no API key required.

```sh
gh repo clone AiSU-AI/aidev ~/code/personal/aidev   # or git clone with SSH
cd ~/code/personal/aidev
./install.sh
```

Then, from inside any Claude Code session:

```
/aidev-run https://github.com/your-org/your-repo/issues/42 ~/code/your-repo
```

...or from your terminal:

```sh
aidev -issue https://github.com/your-org/your-repo/issues/42 -repo ~/code/your-repo
```

---

## What aidev actually does

You give aidev a GitHub issue and a local repo. It runs a sequence of agents that each do one thing well:

1. **Scout** reads your repo (README, `CLAUDE.md`, `ARCHITECTURE.md`, file layout) and produces a factual brief.
2. **Critic** takes the issue + brief + your engineering principles and argues both sides: is this change worth making? It ends with `RECOMMENDATION: build | defer | kill | unclear`. This is the feature aidev is built around — everything else exists because the Critic said *yes*.
3. You approve (or kill).
4. **Architect** produces N meaningfully different solution sketches (default 3) with trade-offs, risks, and principle alignment for each. You pick one.
5. **Implementer** generates a unified git diff. Two turns: it asks which files it needs, you get those, it writes the patch. Saved to `.aidev/proposed.patch` — aidev never touches your source files directly, you `git apply` when you're ready.
6. **Tester** runs your project's detected test suite (go / cargo / pytest / npm / gradle / make). Optionally inside a Docker sandbox.
7. **Reviewer** does a Boy Scout pass on the patch: blockers, suggestions, and proposed follow-up issues.

Every step posts to the issue's GitHub thread as a running audit trail, with one pinned status comment + one-shot artifact comments per phase, and a rotating `aidev:<phase>` label so the Issues page becomes a kanban view of what aidev is doing across every repo.

## Why this shape

Several tools already turn issues into patches (aider, plandex, opencode, goose, Claude Code itself). None of them *argue with you first*. If the Critic decides the feature shouldn't exist, aidev tells you that and stops — it doesn't sheepishly try to implement a thing it thinks is a bad idea. That's the differentiator.

The rest of the pipeline is designed around the same principle: **propose, never mutate**. Implementer writes a diff, doesn't apply it. Reviewer proposes follow-up issues, doesn't file them. Tester can run inside a container if you don't trust the patch yet. Every safety decision defaults to the narrower option.

## Install

**Quick install** (downloads the latest pre-built binary, no Go required):

```sh
curl -fsSL https://raw.githubusercontent.com/AiSU-AI/aidev/main/install.sh | bash -s -- --from-release
```

**From source** (requires Go 1.24+, useful for contributors or if you want to track `main`):

```sh
gh repo clone AiSU-AI/aidev ~/code/personal/aidev    # or: git clone git@github.com:AiSU-AI/aidev.git ~/code/personal/aidev
cd ~/code/personal/aidev
./install.sh
```

The installer:

1. Builds the binary with `go build`
2. Copies it to `~/.local/bin/aidev` (or `~/bin` or `/usr/local/bin`, first writable dir on PATH)
3. Writes default config to `~/.config/aidev/{models,principles}.yaml`
4. Installs Claude Code slash commands to `~/.claude/commands/aidev-*.md`
5. Runs `aidev doctor` to smoke-test the environment
6. Prints next steps

If the install dir isn't on your PATH already, the script will tell you the exact `export PATH=...` line to add to `~/.zshrc` or `~/.bashrc`.

### Flags

```sh
./install.sh --bin ~/bin        # override target bin directory
./install.sh --from-source      # force build mode (the default when Go is present)
./install.sh --from-release     # download from a GitHub release (requires the repo to be public OR GITHUB_TOKEN with contents:read access)
./install.sh --version v0.2f    # pin a specific release tag (release mode only)
./install.sh --force            # overwrite existing binary/config/plugin files
./install.sh --no-doctor        # skip the post-install smoke test
./install.sh --no-prereqs       # skip the 'suggest installing ollama' check
```

### Future: one-line install via release binaries

The install script has a release-download mode that would let anyone run:

```sh
curl -sSL https://raw.githubusercontent.com/AiSU-AI/aidev/main/install.sh | bash
```

without cloning the repo or having Go installed. That flow only works when `AiSU-AI/aidev` is public or when the user has a `GITHUB_TOKEN` with read access. The `v*` tag → GitHub Actions → release-asset pipeline is in place (`.github/workflows/release.yaml`); once the repo goes public, the `curl | bash` one-liner becomes the canonical install path and replaces the clone-first flow above.

### Requirements

- **macOS or Linux.** Windows is not supported yet — the TUI has POSIX-isms.
- **Go 1.24+** to build from source. Install from https://go.dev/dl/.
- **[Claude Code](https://claude.ai/download)** installed and logged in (`claude /login`). aidev's medium and large tiers route through `claude --print`, so agent calls bill against your Max/Pro subscription. No `ANTHROPIC_API_KEY` required for the default config.
- **[Ollama](https://ollama.com)** for the small tier (Scout, Tester failure summary). Optional — you can route the small tier through the claude CLI too by editing `config/models.yaml`, at the cost of subscription tokens.
- **`GITHUB_TOKEN`** in your environment for reading private issues and posting the audit trail (`Issues: Read and write` scope). The same token is also used for `git push` if your repo access is HTTPS-based.

Run `aidev doctor` at any point to verify the environment. If Ollama is installed but not running, doctor will spawn `ollama serve` in the background automatically.

## Usage

### Typical flow (interactive TUI)

```sh
aidev -issue https://github.com/your-org/your-repo/issues/42 -repo ~/code/your-repo
```

| Key | Action |
|-----|--------|
| `r` | Run Scout + Critic |
| `a` | Approve Critic's recommendation → kick off Architect |
| `1`–`9` | After sketches appear, pick one → kick off Implementer |
| `t` | After patch is ready: run the test suite |
| `v` | After patch is ready: run the Reviewer |
| `k` | Kill the proposal |
| `tab` | Cycle pane focus |
| `q` / `ctrl+c` | Quit |

The rightmost pane retitles as the pipeline advances: **Critic report** → **Architect sketches** → **Implementer patch** → **Test result** → **Review**. Earlier phases live in the GitHub audit trail on the issue, so the TUI doesn't try to preserve history.

### From inside Claude Code (slash commands)

Once `aidev plugin install` has dropped the slash commands, type `/` in any Claude Code session and you'll see:

| Command | Effect |
|---|---|
| `/aidev-run <issue-url> <repo-path>` | Full pipeline: scout → critic → architect → sketch 1 → implementer |
| `/aidev-doctor` | Environment audit |
| `/aidev-charter <repo-path>` | 5-question interview to produce `.aidev/charter.md` |
| `/aidev-test <repo-path>` | Run detected test suite, summarise failures |
| `/aidev-review <issue-url> <repo-path>` | Boy Scout pass on the current proposed patch |

### Headless (CI-friendly)

```sh
# Scout + Critic only
aidev -headless -issue <url> -repo <path>

# + auto-run Architect when Critic recommends "build"
aidev -headless -auto -n 3 -issue <url> -repo <path>

# + auto-run Implementer on sketch 1
aidev -headless -auto -sketch 1 -issue <url> -repo <path>
```

Writes the diff to `.aidev/proposed.patch` and prints the full Markdown report to stdout.

### Standalone subcommands

```sh
aidev doctor                                # environment audit
aidev charter -repo <path>                  # interactive product charter interview
aidev clarify -issue <url> -repo <path>     # structured question graph for ambiguities
aidev test -repo <path>                     # run detected test suite
aidev test -repo <path> --sandbox --image golang:1.24   # run tests inside Docker
aidev review -issue <url> -repo <path>      # Boy Scout pass on .aidev/proposed.patch
aidev followups -repo <path>                # dry-run review of proposed follow-up issues
aidev followups -repo <path> --file-issues --target owner/repo   # actually file them
aidev plugin install                        # install Claude Code slash commands
aidev install                               # (re)install default config to ~/.config/aidev
aidev --version                             # print the embedded build version
```

## Configuration

### `~/.config/aidev/models.yaml`

Three tiers (`small`, `medium`, `large`) mapped to providers (`ollama`, `anthropic`, `claude-cli`), plus a role-to-tier routing table. The shipped defaults:

```yaml
tiers:
  small:  { provider: ollama,     model: qwen2.5-coder:7b,  ... }
  medium: { provider: claude-cli, model: "" }
  large:  { provider: claude-cli, model: "" }
routing:
  scout: small
  critic: large
  architect: large
  charter: large
  clarifier: large
  implementer: medium
  reviewer: medium
  tester: small
```

Want the Critic on the direct Anthropic API with a specific model? Change one line:

```yaml
large: { provider: anthropic, model: claude-opus-4-6 }
```

and export `ANTHROPIC_API_KEY`. Want Scout on Claude instead of local Ollama? Change `scout: small` to `scout: large`.

### `~/.config/aidev/principles.yaml`

The living charter the Critic uses when arguing whether a feature should ship. Starts with "Should this exist?", Boy Scout Rule, DRY, YAGNI, Well-Architected, Single Responsibility, Fail Loudly at Boundaries, Tests Describe Intent, and Reversibility. Add your own; the Critic cites them by name.

A target repo can also ship its own `.aidev/principles.yaml` which is merged on top of the global set — useful when an organisation has a standards repo (`ai-dev-standards`) that downstream projects inherit from.

### `.aidev/charter.md` (per-repo)

When a target repo has no strong signal — no README, no `CLAUDE.md`, no `ARCHITECTURE.md` — the Critic has nothing to anchor against. Run `aidev charter -repo <path>` to sit through a 5-question interview and produce `<repo>/.aidev/charter.md`. The Scout automatically absorbs it on every subsequent run and the Critic cites it when pushing back on proposals.

### `.aidev/clarifier.md` (per-repo, per-session)

After the Critic flags ambiguities in a proposal, run `aidev clarify -issue ... -repo ...` to get a structured dependency-aware question graph. Independent questions batch, chained questions serialise. Your answers get written to `.aidev/clarifier.md` and the next Architect run absorbs them as context.

## The full agent pipeline

```
┌─────────────────────────────────────────────────────────────────────┐
│ Orchestrator (state machine + Reporter fanout)                      │
└──┬──────┬──────┬─────┬──────┬──────┬─────┬──────┬──────┬────────────┘
   │      │      │     │      │      │     │      │      │
 Scout  Critic  (Clarifier) Architect Charter Implementer Tester Reviewer
 (sm)   (lg)    (lg)        (lg)      (lg)    (md, 2-turn) (sm)  (md)

 sm = small tier, ollama by default
 md = medium tier, claude-cli by default
 lg = large tier,  claude-cli by default
```

- **Scout** (extractive, small tier)
- **Critic** (adversarial, large tier) — the keystone
- **Architect** (divergent sketches, large tier)
- **Charter / Clarifier** (opt-in standalone subcommands)
- **Implementer** (generative, medium tier, two-turn file-content loading)
- **Tester** (detected test runner, small tier for failure summaries, optional Docker sandbox)
- **Reviewer** (Boy Scout pass, medium tier)

## Troubleshooting

| Problem | Fix |
|---|---|
| `aidev doctor` says `FAIL claude-cli-binary` | Install Claude Code from https://claude.ai/download and log in (`claude /login`). Or edit `config/models.yaml` to route medium/large tiers to `anthropic` and set `ANTHROPIC_API_KEY`. |
| `aidev doctor` says `FAIL ollama-daemon` | Should auto-spawn as of v0.2e. If it doesn't, run `ollama serve` manually, or pass `-skip-doctor` if you're managing the daemon yourself. |
| `FAIL ollama-models — missing qwen2.5-coder:7b` | Interactive runs will offer to pull or swap. Manual fix: `ollama pull qwen2.5-coder:7b`, or edit `models.yaml` to point at a model you already have. |
| Agent calls return `not authenticated` | Run `claude /login`. |
| Private repo issue returns 404 | Export `GITHUB_TOKEN` with `Issues: Read and write`. |
| Proposed.patch doesn't `git apply` cleanly | Known rough edge for complex diffs; read it manually and use it as a guide. File an issue with the failing diff if it's consistently bad. |
| Tests touch the working tree in ways you don't trust | `aidev test --sandbox --image golang:1.24` runs inside Docker with the repo read-only. |

## Roadmap

Shipped:

- **v0.1** — Scout + Critic + Bubble Tea TUI
- **v0.2a** — Architect with N divergent sketches, cost preview
- **v0.2b** — `aidev doctor` + Ollama auto-spawn + first-pull consent
- **v0.2b.1** — Controller for automatic GitHub issue audit trail
- **v0.2c** — Charter agent + interview subcommand
- **v0.2d** — Claude Code CLI provider + `aidev plugin install`
- **v0.2e** — One-command `install.sh` + XDG config discovery + prebuilt release binaries
- **v0.3a** — Implementer agent (unified git diff)
- **v0.3a.1** — Two-turn Implementer with file-content loading
- **v0.3a.2** — TUI keybindings for Implementer / Tester / Reviewer
- **v0.3b** — Tester agent with detected test runner
- **v0.4** — Reviewer agent with Boy Scout pass + follow-up proposals
- **v0.4.1** — `aidev followups --file-issues`
- **v0.5a** — Optional Streamer interface + Claude SSE implementation
- **v0.5b** — Tester Docker sandbox
- **v0.5c** — Clarifier agent with dependency-aware question graph

Not yet:

- Windows support (TUI portability)
- `aidev update` self-updater
- SHA256 checksum verification of downloaded release tarballs
- Streaming TUI progress views
- Ollama + Claude CLI Streamer implementations
- Homebrew formula
- PR-comment review-reading loop (aidev reacts to review comments on PRs it opened)

## Layout

```
cmd/aidev/                entry point, flag handling, subcommand dispatch
internal/config/          YAML loader for models + principles
internal/llm/             Provider interface + Ollama/Claude/ClaudeCLI backends + Streamer
internal/github/          REST client (read + write for the audit trail)
internal/repo/            Repo scanner + repo-local principle loader
internal/agents/          Scout · Critic · Architect · Charter · Clarifier ·
                          Implementer · Tester · Reviewer (all pure; side effects
                          live in the orchestrator)
internal/orchestrator/    State machine + Reporter interface + GitHubReporter (controller)
internal/tui/             Bubble Tea model / update / view / keybindings
internal/doctor/          Precondition checks: config, keys, Claude CLI, Ollama
internal/plugin/          Claude Code slash command installer (go:embed)
internal/installpkg/      Default config installer (go:embed)
internal/version/         Build-time version embedding
config/                   Shipped defaults (canonical source)
plugin/aidev/             Slash command source (canonical, also embedded in binary)
install.sh                One-command installer (download + build modes)
Makefile                  Minimal targets for developers who prefer make
.github/workflows/        Release build pipeline (triggered by v* tags)
```

## Contributing

File issues against [`AiSU-AI/aidev`](https://github.com/AiSU-AI/aidev). PRs welcome but start with an issue first — aidev's own Critic may have opinions about whether your proposed change belongs.

## License

See [LICENSE](LICENSE).
