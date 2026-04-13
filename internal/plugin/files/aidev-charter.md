---
description: Run the aidev Charter interview against a target repo
argument-hint: <repo-path>
---

Run the aidev Charter interview against the repository at `$ARGUMENTS`.
This is an interactive subcommand — the user will be prompted for five
questions in their terminal. After it completes, read the resulting
`<repo>/.aidev/charter.md` and summarise its Purpose section for the
user so they can quickly verify it captured their intent.

!`aidev charter -repo $ARGUMENTS`
