---
description: Run the full autonomous /aidev-run pipeline against every issue in a GitHub milestone
argument-hint: <milestone-name> [repo-path]
allowed-tools: Bash(aidev:*), Bash(gh:*), Bash(git:*), Read, Edit, Write, AskUserQuestion
---

Run the full autonomous `/aidev-run` pipeline — Scout → Critic →
Architect → Selector → Claude Code implementation → tests → PR → review
loop — against every open issue in a GitHub milestone, in sequence.
Each issue produces its own PR branched from `origin/preview`. You
review and merge PRs in whatever order you prefer.

Use this AFTER `/aidev-release <milestone>` has assembled the bundle
you want to ship.

STEP 1 — Parse `$ARGUMENTS`. First token (required) is the milestone
name. Second token (optional) is the repo path; default `.` (cwd).

If the milestone name is missing, STOP and ask the user.

STEP 2 — Resolve repo owner/name:

    gh repo view <PATH> --json owner,name --jq '.owner.login + "/" + .name'

STEP 3 — Fetch milestone issues:

    gh issue list --repo <owner>/<repo> --milestone "<name>" \
      --state open --json number,title,labels \
      --jq '[.[] | {number, title, labels: [.labels[].name]}]'

If empty, say "Milestone `<name>` has no open issues; nothing to
implement. Run `/aidev-release <name> <PATH>` first." and STOP.

STEP 4 — Surface the bundle to the user and ask to confirm via
`AskUserQuestion`:

  - **Proceed — run pipeline on all <N> issues** (recommended)
  - **Proceed — but stop after the first issue for a sanity check**
  - **Show me the Critic verdict for each issue first**
  - **Cancel**

If `Show me the Critic verdict`, for each issue run:

    gh issue view <num> --repo <owner>/<repo> --comments \
      | grep -A 20 'Critic report'

and display. Then re-ask the confirmation question.

If `Cancel`, STOP.

STEP 5 — Dirty-tree precheck on the target repo:

    git -C <PATH> status --porcelain

If non-empty, STOP and tell the user to commit/stash/discard their
working-tree changes first. The pipeline will cut feature branches per
issue; dirty tree is dangerous.

STEP 6 — Sync the base branch. Because each `/aidev-run` cuts the
feature branch from `origin/<base>`, we want the base up-to-date:

    git -C <PATH> fetch origin

STEP 7 — Iterate over every issue in the bundle. For each:

  a. Print a progress marker:

         ========================================================
         [<i>/<N>] Issue #<num>: <title>
         ========================================================

  b. Invoke the full `/aidev-run` pipeline for this issue. This is the
     same flow as running `/aidev-run <num> <PATH>` directly — STEPS 1
     through 15 from `aidev-run.md`. You MUST execute all of those
     steps for each issue, including:

       - STEP 2: `aidev -headless -auto -n 3 -issue <num> -repo <PATH>`
       - STEP 6: check for `AIDEV_NEEDS_REFINEMENT=1` marker
       - STEP 7: dirty-tree check (should pass since we checked in STEP 5
         above, but per-issue push commits may leave artifacts — verify)
       - STEP 7.5: cut feature branch via `gh issue develop` from
         `origin/<base>`
       - STEP 8: implement the Selector's chosen sketch natively
       - STEP 9: run the project's test suite
       - STEP 10: commit + push + `gh pr create`
       - STEP 11: post PR-link comment back to the issue
       - STEPS 12-15: autonomous review loop (wait for CI, run triage,
         fix/rebut/defer/escalate, converge)

     See `aidev-run.md` for the full specification of each step.

  c. Record the outcome for the final summary:

       - PR number opened (if reached STEP 10)
       - Final review-loop convergence (`approve` / `rebut_to_ship` /
         `escalate` / `needs-refinement`)
       - Any new `aidev:followup` issues filed during the run
       - Any errors or skipped steps

  d. If the user chose `Proceed — but stop after the first issue` in
     STEP 4, STOP now and go to STEP 9.

  e. Between issues, switch back to the base branch so the next issue's
     `gh issue develop` has a clean starting point:

         git -C <PATH> checkout <base-branch>

     (`<base-branch>` is whatever STEP 7.5 resolved — typically
     `preview`.)

STEP 8 — Continue until all issues are processed.

STEP 9 — Print the final bundle summary:

    === Bundle <name> complete ===
    Processed: <N> issues
    PRs opened: <list of #<PRNUM>>
    Review loop converged (approve): <count>
    Rebutted to ship: <count>
    Escalated (needs human): <count>
    Needs refinement (Critic=unclear): <count>
    Errors: <count>

    Follow-up issues filed during runs:
      - #<num>  <title>

    Next actions:
      - Merge PRs in preferred order (PRs targeting <base-branch>)
      - For escalated PRs: review the escalation comment + resolve manually
      - For needs-refinement issues: edit body + re-run /aidev-triage

    When all PRs merge and the milestone is complete, tag the release:
      git tag v<name> && git push origin v<name>

STEP 10 — Done.

Safety:
  - The bundle runs PRs in PARALLEL in the sense that each PR targets
    the same base branch; they'll have merge conflicts if they touch
    overlapping files. aidev does NOT sequence them by merging the
    previous PR before running the next. You control merge order.
  - Each issue is independent; one failing (escalate, refinement)
    does not abort the batch. The summary shows everything.
  - The first-issue-sanity-check option (STEP 4) is the safe way to
    try this on a new repo — run one end-to-end, verify the PR looks
    right, then re-run for the rest.
  - Total wall-clock time is roughly `N × 5-10 minutes` depending on
    sketch count, test suite duration, and CI latency. For a 5-issue
    bundle, budget 30-60 minutes.

Cost: all autonomous decisions run through your Claude Code
subscription (claude-cli). No Anthropic API budget required.
Implementation is Claude Code native tools — same subscription.
Only cost outside your subscription is GitHub Actions CI minutes for
the review-loop's CI-wait step.

See `aidev-run.md` for the full single-issue pipeline spec.
See `aidev-release.md` for how to assemble a bundle before running
this command.
