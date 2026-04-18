---
description: Run the aidev Reviewer (and optional auto-fix loop) on the current PR
argument-hint: <issue-number-or-url> [repo-path] [--loop]
allowed-tools: Bash(aidev:*), Bash(git:*), Bash(gh:*)
---

Run the aidev Reviewer against the current PR for the given issue.
This is the Boy Scout pass before the user merges. With `--loop`, also
runs the autonomous review→fix→review cycle (P6) until the Reviewer
approves OR aidev escalates to a human.

The patch is auto-discovered from `git diff origin/<base>...HEAD`
where `<base>` is resolved via `<repo>/.aidev/aidev.yaml` →
`pr.base_branch`, then `preview` if it exists on origin, then the
repo's default branch. No more manual `.aidev/proposed.patch`
staging.

STEP 1 — Parse `$ARGUMENTS`. The first token is the issue reference
(URL or bare number). The second token, if present and not `--loop`,
is the repo path. If the repo path is absent, default it to `.`
(cwd). If `--loop` appears anywhere in the arguments, set
`LOOP_MODE=1`.

If `$ARGUMENTS` is empty, STOP and ask the user for an issue
reference.

The `-issue` flag accepts either a full URL or a bare number; the
number is resolved against the repo's git remote. `GITHUB_TOKEN` is
auto-resolved from `gh auth token` so no manual export is needed.

Do NOT use `!` shell execution — use the Bash tool directly.

STEP 2 — When NOT in loop mode, run a single Reviewer pass:

    aidev review -issue <ISSUE> -repo <PATH>

Interpolate the parsed values at the Claude layer. Expand `~` to
`$HOME` if present.

STEP 3 — After the single review completes, report to the user:
  - the **verdict** prominently (approve / changes_requested / comment)
  - any **blockers** the Reviewer found — these MUST be addressed
    before merge
  - any **follow-up issues** the Reviewer proposed — ask the user
    whether they want to file any of them with `gh issue create`
  - note that the follow-ups are also persisted to
    `<runDir>/followups.md` for later triage

STEP 4 — When in `--loop` mode, hand off to the autonomous review→fix
loop. The loop is the same one `/aidev-run` invokes after opening a
PR (see `aidev-run.md` STEPS 12–15). To run it standalone against an
EXISTING PR for `<ISSUE>`:

  1. Discover the PR number for the issue:
     ```
     gh pr list --repo <owner>/<repo> --search "<ISSUE> in:body" --json number,headRefName --jq '.[0]'
     ```
     If multiple PRs exist, prefer the one whose head branch matches
     `aidev/issue-<ISSUE>-*`.

  2. Discover the branch the PR is on (from the previous query) and
     `git -C <PATH> checkout <branch-name>`.

  3. Mark the PR as draft so the loop has room to iterate:
     ```
     gh pr ready <PRNUM> --undo --repo <owner>/<repo>
     ```

  4. Run the loop body — STEPS 12–15 from `aidev-run.md`. Use
     `aidev review -triage -round <R> -issue <ISSUE> -repo <PATH>`
     (plus `-ci-status` and `-prev-actions` as the loop progresses)
     to drive each round. Apply fix_now actions natively; post
     audit comments and rebuttals to the PR via `gh pr comment`;
     converge or escalate per the JSON `convergence` field.

  5. On convergence: post the convergence comment, mark PR ready
     (`gh pr ready <PRNUM>`), summarise to the user.

  6. On escalation: post the escalation comment, add the
     `aidev:needs-human-review` label, leave PR draft, summarise to
     the user with the escalate_reason.

The Triage agent's safety rules apply identically here: rebuts must
cite the chosen Sketch verbatim; security CI findings cannot be
rebutted; protected-path fixes coerce to escalate; cycle-protected
fix attempts coerce to escalate. The `coercions_log` field surfaces
any Go-side overrides — relay them in the audit comment so the
human sees what aidev refused to do.
