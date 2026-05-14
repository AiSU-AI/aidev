---
description: Upgrade aidev to the latest release (or a specific tag)
allowed-tools: Bash(aidev:*)
---

Run aidev's self-update. With no argument, installs the latest published
release. With a tag argument, pins that version.

!`if [ -z "$ARGUMENTS" ]; then aidev upgrade --yes; else aidev upgrade --version "$ARGUMENTS" --yes; fi`

After the swap completes, the running Claude Code session is still using
the old binary in memory — but the next `aidev` invocation (any new
subcommand, `/aidev-run`, `/aidev-review`, etc.) picks up the upgraded
version. Tell the user the new version is live for future calls.

If the user passed a version that doesn't exist, aidev will surface a
clear "no release found with tag X" message — relay that verbatim and
suggest `/aidev-ls` so they can see which tags are available.
