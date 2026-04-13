---
description: Run the detected test suite for a repository via aidev
argument-hint: <repo-path>
---

Run `aidev test` against the repository at `$ARGUMENTS`. aidev will
detect the project's test runner (go/cargo/python/node/gradle/maven/
just/make) and execute it.

!`aidev test -repo $ARGUMENTS`

If the run fails, surface the 3-sentence LLM failure summary at the
top of your response so the user sees the likely cause immediately.
The full output is available for follow-up questions.
