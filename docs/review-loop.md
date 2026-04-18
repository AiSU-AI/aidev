---
title: Review loop
---

# The autonomous review → fix loop

aidev's differentiating feature is that after the initial PR opens, it doesn't stop — the slash command enters a bounded review→fix loop that converges to either `approve`, `rebut_to_ship`, or a documented `escalate` with `aidev:needs-human-review` label.

## The loop, one round

```
1. Wait for CI on the pushed branch (gh pr checks --watch --required).
2. Capture CI status + failure logs to <runDir>/ci-status-round-<R>.json.
3. Run: aidev review -triage -round <R> -issue ... -repo ... -ci-status ...
4. Parse the JSON verdict. Act on `convergence`:
     approve           → post comment, mark PR ready, DONE
     rebut_to_ship     → post each rebuttal + citation, mark ready, DONE
     continue          → apply fix_now actions natively, commit, push, GOTO 1
     escalate          → add aidev:needs-human-review, leave draft, DONE
5. Cap at review.max_rounds (default 3). After the cap, force escalate.
```

**The mandatory first action of every round is `aidev review -triage`.** The slash command never improvises from raw CI logs — the Triage agent is the source of truth for what to do with every finding.

## The Triage agent

Runs after the Reviewer emits findings (per-PR) and CI reports status. Decides per-finding:

| Action | When |
|---|---|
| `fix_now` | Real, actionable, Claude Code can apply a code edit. Provides `fix_plan` naming files/lines. |
| `rebut` | The finding contradicts the chosen Sketch's documented decisions. **Must quote the relevant Sketch section verbatim in `sketch_citation`.** |
| `defer_to_followup` | Real but out of scope for this PR. Slash command files a child issue. |
| `escalate` | Human required. Used when: unclear finding, protected path touched, security check failed, no Sketch citation available for rebuttal. |

## The safety envelope (Go-side coercions)

The LLM's judgment is one input among many. When the LLM and the safety rules disagree, the safety rules win. Every coercion lands in the verdict's `coercions_log` so the humans can see what aidev refused to do.

1. **Rebut without literal `sketch_citation` → escalate.** No exceptions. The Triage prompt instructs the agent to default to `escalate` when it can't cite the Sketch; Go code enforces that.
2. **Rebut with an invented citation (string not present in the chosen Sketch's markdown) → escalate.** Loose-match tolerance (case + whitespace normalised, minimum 12 chars) catches minor paraphrasing; completely-fabricated quotes don't pass.
3. **Rebut on a security-tool CI failure → escalate.** Detected by substring match against `codeql`, `snyk`, `dependabot`, `trivy`, `semgrep`, `npm audit`, `pnpm audit`, `yarn audit`, `osv-scanner`, `ossf scorecard`, `socket security`. Security findings cannot be auto-rebutted regardless of how confident the LLM sounds.
4. **`fix_plan` touching a protected path → escalate.** Default protected paths: `**/migrations/**`, `*.sql`, `**/.github/workflows/**`, `**/secrets/**`, `*.env`, `*.env.*`. Users extend via `.aidev/aidev.yaml` `review.protected_paths`. Fixes to sensitive surfaces never auto-apply.
5. **Cycle protection.** The same finding ID appearing twice across rounds (once with `fix_now`, then again) forces `escalate`. Prevents the loop from chasing its own tail when a fix removes one symptom but introduces another.

## Hooks — the deterministic backstop

The Triage agent plus slash-command logic is load-bearing, but the LLM can still skip it (we observed this early in P6 development — the slash command went ad-hoc and didn't invoke `aidev review -triage` at all). Three hooks installed at `~/.claude/hooks/aidev-*.sh` run regardless of what the LLM decides:

- **`aidev-pre-push.sh`** — PreToolUse on `Bash(git push*)`. When pushing to an `aidev/issue-*` branch, runs `pnpm verify` / `pnpm audit --audit-level=high` (or `go test` + `go vet`, or `cargo test` + `cargo audit`) before the push reaches origin. Blocks the push on failure.
- **`aidev-pr-created.sh`** — PostToolUse on `Bash(gh pr create*)`. Parses the PR URL from stdout, persists it to a session-scoped state file, emits an early warning if CI is already red at PR creation time.
- **`aidev-stop-check.sh`** — Stop event. Reads the session state file, checks every PR created this session: if any has failing CI AND no `aidev:needs-human-review` label, **blocks Claude Code's Stop event** until the human either runs `/aidev-review --loop` or adds the label to acknowledge.

These run at the harness level, not inside the LLM, so they cannot be reasoned-around.

## CI as a peer signal

CI failures feed into Triage alongside Reviewer findings. The verdict's `actions[].source` field is either `reviewer` or `ci`, and the `ci_check` field names the specific check (e.g. `CodeQL`, `Test with Coverage`, `Security Audit`).

Triage treats CI failures with finer discrimination than the Reviewer alone:

- **Failing test with a clear file/line** (e.g. `load-translations.test.ts:51`) → candidate for `fix_now` with concrete `fix_plan`.
- **Security advisory from a tool in the security-check list** → `fix_now` with `fix_plan` pointing at the dependency override mechanism, OR `escalate` if the fix is non-obvious. Never `rebut`.
- **Build failure without an identifiable fault location** → `escalate`. Triage is instructed: "if you'd be inventing architecture, escalate."
- **CI check the PR didn't cause** (failing on files not in the diff) → in v0.3.0 this gets folded into the current PR per the "fix-when-bounded" scope principle (see [PR #536](https://github.com/AiSU-AI/Company-Site/pull/536) for the canonical example: pre-existing `basic-ftp` CVE + `load-translations.test.ts` drift were resolved in-scope rather than filed as new PRs).

## Convergence values

The Triage verdict's `convergence` field drives the slash command's next step:

| Value | Meaning | Slash command action |
|---|---|---|
| `approve` | Reviewer approved, CI green (or no CI checks failed). | Post convergence comment; mark PR ready; done. |
| `rebut_to_ship` | Reviewer asked for changes, every finding has a Sketch-grounded rebuttal, no `fix_now` / `escalate` actions. | Post each rebuttal as a PR comment citing the Sketch section; mark ready; done. |
| `continue` | At least one `fix_now` or `defer_to_followup`. | Apply fixes natively via Edit/Write; run tests; commit; push; re-enter round R+1. |
| `escalate` | At least one `escalate` action, OR cap hit, OR test regression, OR malformed LLM JSON, OR CI timeout. | Post escalation comment with `escalate_reason`; add `aidev:needs-human-review` label; leave PR draft; done. |

The Go-side computation prioritises: `escalate` dominates → `continue` (any `fix_now` / `defer`) → `rebut_to_ship` (only rebuts) → `approve` (clean or empty).

## Rerunning the loop

```
/aidev-review <issue> <repo> --loop
```

Runs the loop standalone against whatever PR exists for the issue, without re-running Scout/Critic/Architect. Useful when a human has pushed a commit to the feature branch and wants aidev to re-triage.

## See also

- [Architecture](architecture.md) — where Triage sits in the broader pipeline
- [Configuration](configuration.md) — `review.*` and `protected_paths` keys
- [Usage](usage.md) — slash commands that drive the loop
