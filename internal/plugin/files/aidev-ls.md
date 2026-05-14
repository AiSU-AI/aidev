---
description: Show installed aidev version and all available releases
allowed-tools: Bash(aidev:*)
---

Show the user which aidev version is installed locally plus the most
recent published releases on GitHub.

- No argument → list defaults.
- Numeric argument → treat as `--limit N`.
- Anything else (e.g. `--all`, `-all`) → pass through as flags.

!`if [ -z "$ARGUMENTS" ]; then aidev ls; elif printf '%s' "$ARGUMENTS" | grep -qE '^[0-9]+$'; then aidev ls --limit "$ARGUMENTS"; else aidev ls $ARGUMENTS; fi`

The legend at the bottom of the output explains the markers:

- `*` next to a tag means it's the version currently installed.
- `→` next to a tag means GitHub marks it as the "Latest" release.

If the user wants to switch to a different release, point them at
`/aidev-upgrade <tag>` (or omit the tag for latest).
