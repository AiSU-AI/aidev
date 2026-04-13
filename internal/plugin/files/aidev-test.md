---
description: Run the detected test suite for a repository via aidev
argument-hint: <repo-path>
allowed-tools: Bash(aidev:*)
---

Run `aidev test` against the repository the user specifies.

If `$ARGUMENTS` is empty, STOP and ask the user which repository they
want to test. Do not default to the current directory — the user is
invoking from inside Claude Code and the cwd is usually the Claude
project, not the aidev target.

When you have a valid path, run:

!`aidev test -repo $ARGUMENTS`

aidev will detect the project's test runner (go/cargo/python/node/
gradle/maven/just/make) and execute it.

If the run fails, surface the 3-sentence LLM failure summary at the
top of your response so the user sees the likely cause immediately.
The full output is available for follow-up questions.
