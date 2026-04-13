---
description: Run the detected test suite for a repository via aidev
argument-hint: <repo-path>
allowed-tools: Bash(aidev:*)
---

Run `aidev test` against the repository the user specifies via
`$ARGUMENTS`.

STEP 1 — Validate `$ARGUMENTS`. If it's empty, STOP and ask the user
which repository they want to test. Do not default to the current
directory — the user is invoking from inside Claude Code and cwd is
usually the Claude project, not the aidev target. Tell them to
re-invoke `/aidev-test <repo-path>`.

Do NOT use `!` shell execution — use the Bash tool directly so the
argument check happens at the Claude layer.

STEP 2 — Once you have a valid repo path, use the Bash tool to run:

    aidev test -repo <THE-PATH>

Interpolate the literal value from `$ARGUMENTS`. Expand `~` to
`$HOME` if present. Wrap the path in double quotes if it contains
spaces.

STEP 3 — After the test run completes, report to the user:
  - whether the run **passed** or **failed**
  - the command aidev detected and ran (from the output's
    `command:` line)
  - if the run failed, the 3-sentence LLM failure summary — surface
    this at the top of your response so the user sees the likely
    cause immediately
  - offer to show the full output if they want more detail
