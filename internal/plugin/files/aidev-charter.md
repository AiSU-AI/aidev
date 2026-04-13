---
description: Run the aidev Charter interview against a target repo
argument-hint: <repo-path>
allowed-tools: Bash(aidev:*)
---

Run the aidev Charter interview against the repository the user
specifies.

If `$ARGUMENTS` is empty, STOP and ask the user which repository they
want to create a charter for. Accept either an absolute or a relative
path. Do not default to the current directory — the charter is
repo-specific and running it against the wrong directory writes a
`.aidev/charter.md` in the wrong place.

When you have a valid path, run:

!`aidev charter -repo $ARGUMENTS`

This is an interactive subcommand — the user will be prompted for five
questions in their terminal. After it completes, read the resulting
`<repo>/.aidev/charter.md` and summarise its Purpose section for the
user so they can quickly verify it captured their intent.
