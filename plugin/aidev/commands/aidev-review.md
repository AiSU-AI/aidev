---
description: Run the aidev Reviewer on the current proposed patch
argument-hint: <issue-url> <repo-path>
allowed-tools: Bash(aidev:*)
---

Run the aidev Reviewer against the patch at `<repo>/.aidev/proposed.patch`
for the given issue. This is the Boy Scout pass before the user
commits the change.

STEP 1 — Parse `$ARGUMENTS` into two whitespace-separated tokens:
  - token 1: the issue URL (https://github.com/owner/repo/issues/N)
  - token 2: the repo path

If either is missing or empty, STOP and ask the user for the missing
values. Tell them to re-invoke
`/aidev-review <issue-url> <repo-path>`.

Do NOT use `!` shell execution — use the Bash tool directly so this
slash command can validate its arguments at the Claude layer.

STEP 2 — Once you have both values, use the Bash tool to run:

    aidev review -issue <THE-URL> -repo <THE-PATH>

Interpolate the literal values from `$ARGUMENTS` into the command
string. Expand `~` to `$HOME` if present. The Bash tool call is
pre-approved by this slash command's `allowed-tools` frontmatter.

STEP 3 — After the review completes, report to the user:
  - the **verdict** prominently (approve / changes_requested / comment)
  - any **blockers** the Reviewer found — these MUST be addressed
    before merge
  - any **follow-up issues** the Reviewer proposed — ask the user
    whether they want to file any of them with
    `aidev followups --file-issues` or by hand via `gh issue create`
  - note that the follow-ups are also persisted to
    `<repo>/.aidev/followups.md` for later triage
