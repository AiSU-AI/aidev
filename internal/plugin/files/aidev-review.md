---
description: Run the aidev Reviewer on the current proposed patch
argument-hint: <issue-url> <repo-path>
allowed-tools: Bash(aidev:*)
---

Run the aidev Reviewer against the patch at `<repo>/.aidev/proposed.patch`
for the given issue. This is the Boy Scout pass before the user commits
the change.

If `$ARGUMENTS` is empty or does not contain two whitespace-separated
tokens, STOP and ask the user for:
  1. the GitHub issue URL
  2. the absolute or relative path to the local repository

When you have both, run:

!`aidev review -issue $1 -repo $2`

After the review completes:
  - Report the **verdict** prominently (approve / changes_requested / comment)
  - List any **blockers** the Reviewer found — these MUST be addressed before merge
  - List any **follow-up issues** the Reviewer proposed — ask the user
    whether they want to file any of them with `gh issue create`
  - The follow-ups are also persisted to `<repo>/.aidev/followups.md`
    for later triage
