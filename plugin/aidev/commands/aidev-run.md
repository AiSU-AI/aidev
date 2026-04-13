---
description: Run the full aidev agent pipeline on a GitHub issue
argument-hint: <issue-url> <repo-path>
allowed-tools: Bash(aidev:*)
---

You are driving `aidev`, the multi-agent coding tool. The user has
invoked this slash command with arguments naming a GitHub issue and a
local repository path.

If `$ARGUMENTS` is empty or does not contain two whitespace-separated
tokens, STOP and ask the user for:
  1. the GitHub issue URL (https://github.com/owner/repo/issues/N)
  2. the absolute or relative path to the local repository

Do not attempt to run aidev without both values — a missing value will
blow up inside the tool with a noisy error.

When you have both, run aidev in headless mode with auto-architect and
sketch 1 picked:

!`aidev -headless -auto -sketch 1 -issue $1 -repo $2`

After the command finishes, summarise the output for the user:
  - what the Critic recommended
  - how many sketches the Architect produced
  - whether the Implementer wrote a patch at `$2/.aidev/proposed.patch`
  - any warnings from the doctor

If the Critic recommended `kill` or `defer`, stop and report the
rationale. Don't push the user to proceed against the Critic's
judgement.
