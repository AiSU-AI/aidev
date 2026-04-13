---
description: Run the aidev Charter interview against a target repo
argument-hint: <repo-path>
allowed-tools: Bash(aidev:*)
---

Run the aidev Charter interview against the repository the user
specifies via `$ARGUMENTS`.

The `!` command below includes a shell-level guard — if the user
invoked `/aidev-charter` with no arguments, it prints a usage line
instead of calling aidev with a broken flag.

!`if [ -n "$ARGUMENTS" ]; then aidev charter -repo "$ARGUMENTS"; else echo "usage: /aidev-charter <repo-path>"; fi`

If the shell above printed a usage line (no repo path was given), ask
the user which repository they want to create a charter for and tell
them to re-invoke `/aidev-charter <path>`. Do not default to the
current directory — the charter is repo-specific and running it
against the wrong directory writes a `.aidev/charter.md` in the wrong
place.

Otherwise, the Charter interview has run. Read the resulting
`<repo>/.aidev/charter.md` and summarise its Purpose section for the
user so they can quickly verify it captured their intent.
