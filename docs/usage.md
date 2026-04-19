---
title: Usage
---

# Usage

The canonical entry point is the Claude Code slash command `/aidev-run` — it drives the full autonomous pipeline (Scout → Critic → Architect → Selector → implementation → test → PR → review loop). The standalone `aidev` CLI exposes each phase as a subcommand for scripting, CI, and one-off operations.

## Slash commands (from inside Claude Code)

Once `aidev plugin install` (part of the installer) has dropped the slash commands, type `/` in any Claude Code session and you'll see:

| Command | Effect |
|---|---|
| `/aidev-run <issue> <repo-path>` | Full autonomous pipeline — Scout → Critic → Architect → Selector → implement → test → PR → review loop |
| `/aidev-review <issue> <repo-path> [--loop]` | Boy Scout pass on the current PR; `--loop` drives the autonomous review→fix cycle against an existing PR |
| `/aidev-triage [repo-path]` | Bulk Critic pass over every un-triaged open issue, applying verdict labels (`aidev:approved` / `deferred` / `killed` / `needs-refinement`) |
| `/aidev-release <milestone> [repo-path]` | Assemble / inspect a release bundle around a GitHub milestone — pick from `aidev:approved` candidates |
| `/aidev-implement-bundle <milestone> [repo-path]` | Run the full autonomous `/aidev-run` pipeline against every open issue in a milestone, producing one PR per issue |
| `/aidev-doctor` | Environment audit |
| `/aidev-charter <repo-path>` | 5-question interview to produce `.aidev/charter.md` |
| `/aidev-test <repo-path>` | Run detected test suite, summarise failures |

`<issue>` accepts either a bare issue number (resolved against the repo's `origin` remote) or a full GitHub URL. `<repo-path>` is a local filesystem path; [URL-as-repo support](https://github.com/AiSU-AI/aidev/issues/45) is on the roadmap.

### Example — full autonomous run

```
/aidev-run 42 .
```

From `~/code/your-repo` this runs the whole pipeline. The Critic either argues against the issue (you'll see the case, with sharp questions on `unclear`), the Architect drafts 3 sketches, the Selector picks one via the [rubric](configuration.md#sketch-rubric), Claude Code cuts the feature branch via `gh issue develop`, implements the sketch natively, runs your test suite, opens the PR, waits for CI, and runs the [review loop](review-loop.md) until it converges or escalates.

### Local issue-management workflow (no CI, no API keys)

For maintainers who want to manage a repo's backlog locally using their Claude Code subscription — no `ANTHROPIC_API_KEY` in GitHub Actions, no CI budget risk:

```
/aidev-triage .                         # Critic every un-labeled open issue
/aidev-release v0.4.0 .                 # assemble a release bundle from approved issues
/aidev-implement-bundle v0.4.0 .        # run the full pipeline on every milestone issue
```

The flow:

1. **Triage** — `/aidev-triage` finds open issues without any `aidev:*` label, runs Scout + Critic on each, applies `aidev:approved` / `aidev:deferred` / `aidev:killed` / `aidev:needs-refinement` based on the Critic's verdict. Cap of 20 issues per run; total run time roughly `N × 60s`.
2. **Release bundling** — `/aidev-release <milestone>` shows you `aidev:approved` issues not yet assigned to a milestone, plus whatever's already in the milestone. Interactive: you pick which candidates to add to the milestone. Creates the milestone if it doesn't exist.
3. **Bundle implementation** — `/aidev-implement-bundle <milestone>` runs the full `/aidev-run` pipeline (Scout → Critic → Architect → Selector → Claude Code → tests → PR → review loop) against every open issue in the milestone, sequentially. One PR per issue, all branched from `origin/<base>`. You merge in whatever order you prefer.

Each step is human-gated at the decision points — `/aidev-triage` surfaces the candidate list before running, `/aidev-release` asks you to confirm each addition, `/aidev-implement-bundle` offers a "stop after the first issue for sanity check" option. The autonomy is opt-in per step, not blanket.

## Headless / CI-friendly

```sh
# Scout + Critic only (produces decision, no implementation):
aidev -headless -issue 42 -repo ~/code/your-repo

# + Architect + Selector autonomously:
aidev -headless -auto -n 3 -issue 42 -repo ~/code/your-repo

# Force a particular Critic verdict (escape hatch; use sparingly):
aidev -headless -auto -n 3 -force-verdict build -issue 42 -repo ~/code/your-repo

# Run an interactive session that pauses for human input at the Architect step:
aidev -interactive -issue 42 -repo ~/code/your-repo
```

Writes the full report to stdout (for CI logs) and drops structured artifacts (Selector verdict, Architect sketches, Clarifier Q&A, CI status) into `$XDG_DATA_HOME/aidev/runs/<owner>-<repo>-<issue>/`.

## Standalone subcommands

```sh
aidev doctor                                          # environment audit
aidev charter -repo <path>                            # interactive product charter interview
aidev clarify -issue <url> -repo <path>               # structured question graph for ambiguities
aidev test -repo <path>                               # run detected test suite
aidev test -repo <path> --sandbox --image golang:1.24 # run tests inside Docker
aidev review -issue <url> -repo <path>                # Boy Scout pass on auto-discovered diff
aidev review -triage -round 1 -issue <url> -repo <path>   # Reviewer + Triage meta-judgment (JSON out)
aidev followups -repo <path>                          # dry-run review of proposed follow-up issues
aidev followups -repo <path> --file-issues --target owner/repo  # actually file them
aidev plugin install                                  # install Claude Code slash commands
aidev install                                         # (re)install default config to ~/.config/aidev
aidev init                                            # interactive first-run: pick a profile + verify prereqs
aidev init --profile cloud-only --yes                 # non-interactive: pick profile, skip prompts (CI)
aidev --version                                       # print the embedded build version
```

### `aidev init` — guided first-run setup

`aidev init` is the recommended entry point after installing the binary. It:

1. Checks prerequisites: `claude` CLI on `PATH`, Ollama on `PATH`, `GITHUB_TOKEN` resolvable via env or `gh auth token`.
2. Prompts you to pick a profile — **cloud-only** (no Ollama, pure Claude Code), **default** (local Ollama + Claude Code hybrid), **high-vram**, **low-vram**, or **offline**.
3. If the picked profile needs Ollama and it's missing, offers to install via `brew install ollama` (macOS with Homebrew) or `curl | sh` (Linux). You can decline and fall back to cloud-only.
4. Writes `~/.config/aidev/models.yaml` with the chosen profile active.
5. Runs `aidev doctor` as a hard gate. If doctor fails, init exits non-zero and points you at the fix.

For scripted callers (CI, Docker builds): `aidev init --profile cloud-only --yes` skips every prompt, fails loudly if prereqs are missing (no install side effects), and exits 0 on success.

Re-running `aidev init` against an existing `~/.config/aidev/models.yaml` refuses to overwrite unless `--force` is passed — this protects hand-edited customizations. Use `aidev config profile use <name>` to switch profiles without rewriting the file.

## Interactive TUI (manual mode)

For phase-by-phase exploration without the autonomous pipeline:

```sh
aidev -issue 42 -repo ~/code/your-repo
```

| Key | Action |
|-----|--------|
| `r` | Run Scout + Critic |
| `a` | Approve Critic's recommendation → kick off Architect |
| `1`–`9` | After sketches appear, pick one → kick off Implementer (standalone mode) |
| `t` | After patch is ready: run the test suite |
| `v` | After patch is ready: run the Reviewer |
| `k` | Kill the proposal |
| `tab` | Cycle pane focus |
| `q` / `ctrl+c` | Quit |

The rightmost pane retitles as the pipeline advances: **Critic report** → **Architect sketches** → **Implementer patch** → **Test result** → **Review**.

## See also

- [Architecture](architecture.md) — what each agent does and why
- [Review loop](review-loop.md) — the autonomous fix/rebut/defer/escalate cycle
- [Configuration](configuration.md) — customize models, principles, rubric
