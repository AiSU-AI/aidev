---
description: Run the aidev Reviewer on the current proposed patch
argument-hint: <issue-url> <repo-path>
allowed-tools: Bash(aidev:*)
---

Run the aidev Reviewer against the patch at `<repo>/.aidev/proposed.patch`
for the given issue. This is the Boy Scout pass before the user
commits the change.

The `!` command below uses `set --` to split `$ARGUMENTS` into shell
positional parameters, then guards on both being non-empty. If either
is missing, it prints a usage line instead of calling aidev with a
broken flag.

!`set -- $ARGUMENTS; if [ -n "$1" ] && [ -n "$2" ]; then aidev review -issue "$1" -repo "$2"; else echo "usage: /aidev-review <issue-url> <repo-path>"; fi`

If the shell above printed a usage line, ask the user for the missing
values and tell them to re-invoke
`/aidev-review <issue-url> <repo-path>`.

Otherwise, the Reviewer has completed. Report to the user:
  - the **verdict** prominently (approve / changes_requested / comment)
  - any **blockers** the Reviewer found — these MUST be addressed
    before merge
  - any **follow-up issues** the Reviewer proposed — ask the user
    whether they want to file any of them with
    `aidev followups --file-issues` or by hand via `gh issue create`
  - the follow-ups are also persisted to `<repo>/.aidev/followups.md`
    for later triage
