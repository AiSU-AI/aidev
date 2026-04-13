---
description: Run the aidev Reviewer on the current proposed patch
argument-hint: <issue-number-or-url> [repo-path]
allowed-tools: Bash(aidev:*)
---

Run the aidev Reviewer against the patch at `<repo>/.aidev/proposed.patch`
for the given issue. This is the Boy Scout pass before the user
commits the change.

STEP 1 — Parse `$ARGUMENTS`. The first token is the issue reference
(URL or bare number). The second token, if present, is the repo
path. If the repo path is absent, default it to `.` (cwd).

If `$ARGUMENTS` is empty, STOP and ask the user for an issue
reference.

The `-issue` flag accepts either a full URL or a bare number; the
number is resolved against the repo's git remote. `GITHUB_TOKEN` is
auto-resolved from `gh auth token` so no manual export is needed.

Do NOT use `!` shell execution — use the Bash tool directly.

STEP 2 — Use the Bash tool to run:

    aidev review -issue <ISSUE> -repo <PATH>

Interpolate the parsed values at the Claude layer. Expand `~` to
`$HOME` if present.

STEP 3 — After the review completes, report to the user:
  - the **verdict** prominently (approve / changes_requested / comment)
  - any **blockers** the Reviewer found — these MUST be addressed
    before merge
  - any **follow-up issues** the Reviewer proposed — ask the user
    whether they want to file any of them with
    `aidev followups --file-issues` or by hand via `gh issue create`
  - note that the follow-ups are also persisted to
    `<repo>/.aidev/followups.md` for later triage
