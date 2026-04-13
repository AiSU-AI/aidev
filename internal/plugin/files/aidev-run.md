---
description: Run the full aidev agent pipeline on a GitHub issue
argument-hint: <issue-url> <repo-path>
allowed-tools: Bash(aidev:*)
---

You are driving `aidev`, the multi-agent coding tool. The user invoked
this slash command expecting two arguments in `$ARGUMENTS`: a GitHub
issue URL and a local repository path.

The `!` command below uses `set --` to split `$ARGUMENTS` into shell
positional parameters, then guards on both being non-empty. If either
is missing, it prints a usage line instead of calling aidev with a
broken flag.

!`set -- $ARGUMENTS; if [ -n "$1" ] && [ -n "$2" ]; then aidev -headless -auto -sketch 1 -issue "$1" -repo "$2"; else echo "usage: /aidev-run <issue-url> <repo-path>"; fi`

If the shell above printed a usage line, ask the user for the missing
values and tell them to re-invoke
`/aidev-run <issue-url> <repo-path>`. Do NOT try to re-run the command
yourself with guessed arguments — the user decides which issue to
work on.

Otherwise, the full pipeline has run. Summarise the output for the
user:
  - what the Critic recommended
  - how many sketches the Architect produced
  - whether the Implementer wrote a patch at
    `<repo>/.aidev/proposed.patch`
  - any warnings from the doctor

If the Critic recommended `kill` or `defer`, stop and report the
rationale. Don't push the user to proceed against the Critic's
judgement.
