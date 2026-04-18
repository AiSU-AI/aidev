---
title: Troubleshooting
---

# Troubleshooting

| Problem | Fix |
|---|---|
| `aidev doctor` says `FAIL claude-cli-binary` | Install Claude Code from [claude.ai/download](https://claude.ai/download) and log in (`claude /login`). Or edit `~/.config/aidev/models.yaml` to route medium/large tiers to `anthropic` and set `ANTHROPIC_API_KEY`. |
| `aidev doctor` says `FAIL ollama-daemon` | `doctor` should auto-spawn Ollama as of v0.2e. If it doesn't, run `ollama serve` manually, or pass `-no-doctor` if you're managing the daemon yourself. |
| `FAIL ollama-models — missing qwen2.5-coder:7b` | Interactive runs offer to pull or swap. Manual: `ollama pull qwen2.5-coder:7b`, or edit `models.yaml` to point at a model you already have. |
| Agent calls return `not authenticated` | Run `claude /login`. |
| Private-repo issue returns 404 | Export `GITHUB_TOKEN` with `Issues: Read and write` scope. Auto-resolved from `gh auth token` if you've run `gh auth login`. |
| `/aidev-run` stops with `AIDEV_NEEDS_REFINEMENT=1` | Selector refused to pick, or Critic returned `unclear` after the Clarifier interview. See the issue's new `🤔 refinement required` comment for the structured checklist. Refine the issue body, then re-run. |
| Review loop escalated with `aidev:needs-human-review` label | Triage hit a blocker it couldn't safely auto-fix (security finding, protected path, cycle protection). Address manually OR push a commit that changes the finding fingerprint, then re-run `/aidev-review --loop`. |
| Pre-push hook blocks on `pnpm verify` failure from an unrelated CI issue | Temporarily rerun with `git push --no-verify` (discouraged) or, better, fold the unrelated fix into the same PR per aidev's scope-when-bounded principle. See [review loop](review-loop.md#ci-as-a-peer-signal). |
| Selector chose a sketch with low confidence (score barely > 0) | Increase sketch count with `-n 5` for more diversity, or set `min_implementable_score: 1.0` in `<repo>/.aidev/sketch-rubric.yaml` to force refinement on borderline cases. |
| Tests touch the working tree in ways you don't trust | `aidev test --sandbox --image golang:1.24` runs inside Docker with the repo read-only. |
| Generated artifacts cluttering `<repo>/.aidev/` | Shouldn't happen in v0.3.0+; artifacts now land in `$XDG_DATA_HOME/aidev/runs/<owner>-<repo>-<issue>/`. If you see them in the target repo, reinstall: `curl -fsSL https://raw.githubusercontent.com/AiSU-AI/aidev/main/install.sh \| bash -s -- --from-release --force`. |
| Stop hook blocks Claude Code session end with "session PR has failing CI" | Expected when a fresh PR this session has red CI. Options: (1) run `/aidev-review --loop` to trigger the autonomous fix cycle; (2) `gh pr edit <PRNUM> --add-label aidev:needs-human-review` to acknowledge. |

## When to file an issue

If the above doesn't resolve your problem, or if you've hit an obvious bug (pipeline crashes, Critic output is malformed, Selector picks a disqualified sketch, etc.), file an issue at [AiSU-AI/aidev/issues](https://github.com/AiSU-AI/aidev/issues) with:

- The exact `aidev` command you ran (including flags)
- The output of `aidev doctor`
- The `<runDir>/architect-output.md` contents if the pipeline reached the Architect phase
- Your `~/.config/aidev/models.yaml` (redact API keys)

## See also

- [Install](install.md) — `aidev doctor` reference
- [Configuration](configuration.md) — principles, rubric, per-repo overrides
- [Review loop](review-loop.md) — escalation behavior + hook backstop
