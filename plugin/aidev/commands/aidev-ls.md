---
description: Show installed aidev version and all available releases
allowed-tools: Bash(aidev:*)
---

Show the user which aidev version is installed locally plus the most
recent published releases on GitHub.

With no argument:

!`aidev ls`

With a numeric argument (interpreted as a limit):

!`aidev ls --limit "$ARGUMENTS"`

The legend at the bottom of the output explains the markers:

- `*` next to a tag means it's the version currently installed.
- `→` next to a tag means GitHub marks it as the "Latest" release.

If the user wants to switch to a different release, point them at
`/aidev-upgrade <tag>` (or omit the tag for latest).
