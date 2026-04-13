---
description: Run the detected test suite for a repository via aidev
argument-hint: <repo-path>
allowed-tools: Bash(aidev:*)
---

Run `aidev test` against the repository the user specifies via
`$ARGUMENTS`.

The `!` command below includes a shell-level guard — if the user
invoked `/aidev-test` with no arguments, it prints a usage line
instead of calling aidev with a broken flag.

!`if [ -n "$ARGUMENTS" ]; then aidev test -repo "$ARGUMENTS"; else echo "usage: /aidev-test <repo-path>"; fi`

If the shell above printed a usage line, ask the user which
repository they want to test and tell them to re-invoke
`/aidev-test <path>`. Do not default to the current directory — the
user is invoking from inside Claude Code and the cwd is usually the
Claude project, not the aidev target.

Otherwise, aidev has detected and run the project's test suite
(go/cargo/python/node/gradle/maven/just/make). If the run failed,
surface the 3-sentence LLM failure summary at the top of your response
so the user sees the likely cause immediately. The full output is
available for follow-up questions.
