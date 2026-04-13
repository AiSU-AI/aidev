---
description: Run the full aidev agent pipeline on a GitHub issue
argument-hint: <issue-url> <repo-path>
allowed-tools: Bash(aidev:*)
---

You are driving `aidev`, the multi-agent coding tool. The user invoked
this slash command expecting two arguments in `$ARGUMENTS`: a GitHub
issue URL and a local repository path.

STEP 1 — Parse `$ARGUMENTS` into two whitespace-separated tokens:
  - token 1: the issue URL (https://github.com/owner/repo/issues/N)
  - token 2: the repo path (absolute, or starting with `~`)

If either is missing or empty, STOP and ask the user for the missing
values. Do NOT proceed with partial input. Do NOT guess a repo path
from the current working directory — the user is invoking from inside
Claude Code, so cwd is almost never the intended aidev target. Tell
them to re-invoke `/aidev-run <issue-url> <repo-path>`.

Do NOT use `!` shell execution in this slash command. `!` runs before
the prompt body is processed, and that means Claude Code can't
validate the arguments first. Use the Bash tool directly instead.

STEP 2 — Once you have both values, use the Bash tool to run ONE
aidev command with the two arguments interpolated into the command
string at the Claude layer (not as shell variables):

    aidev -headless -auto -sketch 1 -issue <THE-URL> -repo <THE-PATH>

Where `<THE-URL>` and `<THE-PATH>` are the literal strings you parsed
from `$ARGUMENTS`, with `~` expanded to `$HOME` if present. Wrap the
path in double quotes if it contains spaces or shell-special
characters.

The Bash tool call itself is pre-approved by this slash command's
`allowed-tools: Bash(aidev:*)` frontmatter, so it will not trip the
permission layer as long as the command starts with `aidev`.

STEP 3 — After the command finishes, summarise the output for the
user:
  - what the Critic recommended (build / defer / kill / unclear)
  - how many sketches the Architect produced
  - whether the Implementer wrote a patch at
    `<repo>/.aidev/proposed.patch`
  - any doctor warnings

If the Critic recommended `kill` or `defer`, stop and report the
rationale. Don't push the user to proceed against the Critic's
judgement — that's the entire point of the Critic.
