---
description: Run the aidev Reviewer (and optional auto-fix loop) on the current PR
argument-hint: <issue-number-or-url> [repo-path] [--loop]
allowed-tools: Bash(aidev:*), Bash(git:*), Bash(gh:*)
---

Run the aidev Reviewer against the current PR for the given issue.
This is the Boy Scout pass before merge. With `--loop`, also runs
the autonomous review→fix→review cycle (P6) until the Reviewer
approves OR aidev escalates to a human.

The patch is auto-discovered from `git diff origin/<base>...HEAD`
where `<base>` is resolved via `<repo>/.aidev/aidev.yaml` →
`pr.base_branch`, then `preview` if it exists on origin, then the
repo's default branch. No more manual `.aidev/proposed.patch` staging.

STEP 1 — Parse `$ARGUMENTS`. The first token is the issue reference
(URL or bare number). The second token, if present and not `--loop`,
is the repo path. If the repo path is absent, default it to `.` (cwd).
If `--loop` appears anywhere in the arguments, set `LOOP_MODE=1`.

If `$ARGUMENTS` is empty, STOP and ask the user for an issue reference.

The `-issue` flag accepts either a full URL or a bare number; the
number is resolved against the repo's git remote. `GITHUB_TOKEN` is
auto-resolved from `gh auth token` so no manual export is needed.

Do NOT use `!` shell execution — use the Bash tool directly.

STEP 2 (no --loop) — Single Reviewer pass:

    aidev review -issue <ISSUE> -repo <PATH>

Interpolate the parsed values at the Claude layer. Expand `~` to
`$HOME` if present.

STEP 3 (no --loop) — Report the verdict + blockers + follow-ups to
the user. Follow-ups land in `<runDir>/followups.md`. Done.

=============================================================
STEP 4 (--loop MODE) — THE AUTONOMOUS REVIEW→FIX LOOP
=============================================================

This section is the full STEPS 12–15 body from `aidev-run.md`,
inlined here so you do NOT need to read another file and do NOT
improvise. **Your very first action in this mode MUST be STEP 4d
below** — running `aidev review -triage`. Do not read CI logs
yourself, do not grep for vulnerabilities, do not decide to edit
files before you have the JSON verdict in hand. The Triage agent
is the source of truth for what to do with every finding; your job
is to execute its decisions, not to duplicate its work.

STEP 4a — Discover the PR for this issue and check it out:

    gh pr list --repo <owner>/<repo> --search "<ISSUE> in:body" \
      --json number,headRefName --jq '.[0]'

Prefer the PR whose `headRefName` matches `aidev/issue-<ISSUE>-*`.
Capture `<PRNUM>` and `<branch-name>`. Then:

    git -C <PATH> fetch origin <branch-name>
    git -C <PATH> checkout <branch-name>

If no PR exists for this issue, STOP — the loop operates on an
existing PR, not a branch without one. Tell the user to open the
PR first (or run `/aidev-run <ISSUE> <PATH>` instead).

STEP 4b — Mark the PR as draft so the loop has room to iterate:

    gh pr ready <PRNUM> --undo --repo <owner>/<repo>

STEP 4c — Resolve loop config from `<PATH>/.aidev/aidev.yaml` (or
use defaults):
  - `review.max_rounds` — default 3
  - `review.wait_for_ci` — default true
  - `review.ci_timeout_minutes` — default 30
  - `review.protected_paths` — default: `**/migrations/**`, `*.sql`,
    `**/.github/workflows/**`, `**/secrets/**`, `*.env`, `*.env.*`

Initialise loop state in memory:
  - `round = 1`
  - `prevActions = []`  (TriageAction objects, accumulates across rounds)

STEP 4d — Run ONE review round. This is the mandatory loop body;
every round starts here.

  1. **Wait for CI** (when `review.wait_for_ci` is true and a push
     has happened in this session). For the FIRST round against an
     existing PR, skip the wait — CI has already run. For
     subsequent rounds after your own pushes:
     ```
     gh pr checks <PRNUM> --repo <owner>/<repo> --watch --required --interval 30
     ```
     Honour `ci_timeout_minutes`: if the command doesn't return
     within the timeout, treat as `escalate` convergence.

  2. **Capture CI status** for Triage. Write
     `<runDir>/ci-status-round-<R>.json`:
     ```json
     {
       "checks": [
         {"name": "build", "state": "completed", "conclusion": "success"},
         {"name": "CodeQL", "state": "completed", "conclusion": "failure",
          "details_url": "https://...",
          "log_excerpt": "<last ~50 lines from `gh run view <id> --log-failed`>"}
       ]
     }
     ```
     Use `gh pr checks <PRNUM> --json name,state,conclusion,detailsUrl`
     for the index; for each check with `conclusion=failure`, run
     `gh run view <run-id> --log-failed --repo <owner>/<repo> | tail -50`
     and put the output in `log_excerpt`.

  3. **Persist prior actions** (cycle protection). If `round > 1`,
     write `<runDir>/prev-actions.json`:
     ```json
     [{"id": "f1", "action": "fix_now"}, ...]
     ```
     Skip on round 1.

  4. **Run aidev with triage — this is MANDATORY.** You MUST
     capture the JSON output of this command and drive all
     subsequent decisions from it. Do NOT inspect CI logs manually,
     do NOT grep for vulnerabilities, do NOT propose fixes based on
     your own reading. The Triage agent has read the chosen Sketch
     and will tell you exactly what to do with every finding:

     ```
     aidev review -triage \
       -issue <ISSUE> \
       -repo <PATH> \
       -round <R> \
       -ci-status <runDir>/ci-status-round-<R>.json \
       -prev-actions <runDir>/prev-actions.json
     ```

     Output is a single JSON object:
     ```json
     {
       "round": 1,
       "review_verdict": "approve|changes_requested|comment",
       "convergence": "approve|rebut_to_ship|continue|escalate",
       "actions": [...],
       "coercions_log": [...],
       "escalate_reason": "..."
     }
     ```

     Parse it. From here on, the `convergence` field is your
     decision tree — not your own judgment, not CI logs, not the
     Reviewer markdown.

STEP 4e — Act on `convergence`:

**`approve`:** post the convergence comment, mark PR ready, DONE.

    gh pr comment <PRNUM> --repo <owner>/<repo> --body "$(cat <<'EOF'
    ## 🟢 Auto-review converged at round <R>

    Reviewer verdict: approve. CI: green. No findings to address.

    PR is ready for human review.
    EOF
    )"
    gh pr ready <PRNUM> --repo <owner>/<repo>

BREAK loop.

**`rebut_to_ship`:** ALL findings have Sketch-grounded rebuttals.
Do NOT edit files. For each `action: rebut` in the verdict, post
a per-rebuttal comment to the PR:

    gh pr comment <PRNUM> --repo <owner>/<repo> --body "$(cat <<'EOF'
    ## ↩️ Round <R> rebuttal: finding <id>

    **Reviewer finding:** <finding>

    **aidev's rebuttal:** <rebut_text>

    **Grounded in Sketch <N>:** > <sketch_citation>
    EOF
    )"

Then post the convergence comment + mark ready:

    gh pr comment <PRNUM> --repo <owner>/<repo> --body "$(cat <<'EOF'
    ## 🟢 Auto-review converged at round <R> (rebut_to_ship)

    All <N> Reviewer findings rebutted with Sketch-grounded rationale.
    See the per-finding comments above for the audit trail.
    EOF
    )"
    gh pr ready <PRNUM> --repo <owner>/<repo>

BREAK loop.

**`continue`:** ONE OR MORE findings need action. For each action
in the verdict:

  a. **`action: fix_now`** — apply the fix natively using Edit/Write
     based on the `fix_plan` field. Append to the in-memory
     decisions journal: "Round <R> fix (id=<id>): <fix_plan one-liner>".

  b. **`action: rebut`** — post the per-finding rebuttal comment
     (same template as `rebut_to_ship` above). No file changes.

  c. **`action: defer_to_followup`** — file a child issue:
     ```
     gh issue create --repo <owner>/<repo> \
       --title "[follow-up #<ISSUE>] <one-line summary>" \
       --label aidev:followup \
       --body "<finding>\n\n_Filed automatically by aidev review-loop round <R>. Parent: #<ISSUE>, PR: #<PRNUM>._"
     ```
     Capture the new issue number; back-link in the PR.

  d. **`action: escalate`** — do NOT edit files for this finding.
     Collect it for the escalation handler below (if ANY action is
     escalate, the whole round converges to escalate — this branch
     shouldn't be reached with mixed actions, but guard anyway).

After applying all fix_now actions, **run the test suite** (same
detection as STEP 9 of aidev-run: `pnpm verify` / `go test ./...` /
etc.). If tests fail:
  - `git -C <PATH> checkout -- <files-modified-this-round>` — REVERT.
  - Append the failure to the decisions journal.
  - Treat the round as `escalate` convergence (jump below).
  - Do NOT commit the broken fix.

If tests pass:
  - `git -C <PATH> add <files-modified-this-round>`
  - `git -C <PATH> commit -m "fix: address review round <R> - <one-line summary>"`
  - `git -C <PATH> push origin <branch-name>`
  - Update `prevActions` in memory: append `{id, action}` for every
    finding processed this round.

Post the round audit comment to the PR:

    gh pr comment <PRNUM> --repo <owner>/<repo> --body "$(cat <<'EOF'
    ## 🔁 Auto-review round <R>: continue

    **Reviewer verdict:** <review_verdict>
    **Triage convergence:** continue
    **Findings:** <N> total

    | # | Source | Severity | Action | Notes |
    |---|--------|----------|--------|-------|
    | f1 | reviewer | blocker | fix_now | Applied edit to src/foo.ts:42 |
    | ci-1 | ci (CodeQL) | blocker | fix_now | Replaced .includes() with URL constructor |
    | f2 | reviewer | suggestion | rebut | Sketch §"Outlier handling" |
    | f3 | reviewer | followup | defer_to_followup | Filed #<NEW> |

    Fixes pushed in commit `<short-sha>`. Re-running review next round.

    <If verdict had coercions_log entries, include them here verbatim
    so the user sees what aidev refused to do.>
    EOF
    )"

Increment `round`. If `round > max_rounds`, treat as `escalate`.
Otherwise GOTO STEP 4d.

**`escalate`:** Stop the loop. Post the escalation comment, add
the `aidev:needs-human-review` label, leave PR draft:

    gh pr comment <PRNUM> --repo <owner>/<repo> --body "$(cat <<'EOF'
    ## 🚨 Auto-review escalated at round <R>

    **Why:** <escalate_reason from the JSON>

    Findings requiring human attention:
    - **<id>** (<source>, <severity>): <finding>
      - **Action:** escalate. <rationale>

    [Repeat per escalate action]

    The PR is left as draft. Working tree on `<branch-name>` is in
    a known-good state. Resolve the findings manually or re-run
    `/aidev-run <ISSUE> <PATH>` after addressing them.
    EOF
    )"
    gh pr edit <PRNUM> --repo <owner>/<repo> --add-label aidev:needs-human-review

BREAK loop.

STEP 4f — Final summary. After loop exit, print to the user:
  - "Auto-review loop converged at round <R> with `<convergence>`."
    OR
  - "Auto-review loop escalated at round <R>. PR #<PRNUM> is draft;
    `aidev:needs-human-review` label applied. See the escalation
    comment for next steps."

Loop safety (enforced by aidev's Triage agent, not you):
  - Rebuttals require literal Sketch citations; ungrounded rebuts
    coerce to escalate.
  - Security-tool CI failures (CodeQL, Snyk, Dependabot, Trivy,
    Semgrep, npm/pnpm audit) CANNOT be rebutted; coerce to fix_now
    or escalate.
  - Fixes touching `review.protected_paths` coerce to escalate.
  - The same finding ID with `fix_now` in two rounds coerces to
    escalate (cycle protection).

These coercions appear in `coercions_log` — relay them in the
round audit comment so the human sees what aidev refused to do.

⚠️ REMINDER — the one failure mode that must not happen: acting on
CI logs, Reviewer markdown, or your own judgment **before** calling
`aidev review -triage`. If you find yourself grepping `gh run view`
output or writing Edit calls without a JSON verdict in hand, STOP
and run STEP 4d.1 first.
