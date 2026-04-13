---
description: Run the full aidev agent pipeline on a GitHub issue
argument-hint: <issue-number-or-url> [repo-path]
allowed-tools: Bash(aidev:*)
---

You are driving `aidev`, the multi-agent coding tool.

STEP 1 — Parse `$ARGUMENTS` into tokens. The first token is the issue
reference (either a full GitHub URL or a bare issue number). The
second token, if present, is the repository path. If the repo path
is absent, default it to `.` (current working directory) — aidev can
resolve a bare issue number against the cwd's git remote.

If `$ARGUMENTS` is empty, STOP and ask the user for an issue
reference. Do NOT proceed with no input.

aidev's `-issue` flag accepts either form:
  - full URL: `https://github.com/owner/repo/issues/123`
  - bare number: `123` (resolved against `-repo`'s git remote origin)

aidev's `GITHUB_TOKEN` is auto-resolved from `gh auth token` when the
env var isn't set, so you do not need to wrap the command with a
manual credential export. If you see a 'private repo? set
GITHUB_TOKEN' error anyway, tell the user to run `gh auth login` (or
export a PAT) and retry.

STEP 2 — Use the Bash tool to run ONE aidev command with the parsed
values interpolated at the Claude layer (not as shell variables):

    aidev -headless -auto -sketch 1 -issue <ISSUE> -repo <PATH>

Where `<ISSUE>` is the literal first token from `$ARGUMENTS` and
`<PATH>` is the literal second token (or `.` if unset). Expand `~`
to `$HOME`. Wrap the path in double quotes if it contains spaces.

The Bash call matches `Bash(aidev:*)` and passes the permission
layer cleanly because it's a single `aidev` invocation with no
surrounding shell logic.

STEP 3 — After the command finishes, summarise the output for the
user:
  - what the Critic recommended (build / defer / kill / unclear)
  - how many sketches the Architect produced (if build)
  - whether the Implementer wrote a patch at
    `<repo>/.aidev/proposed.patch`
  - any doctor warnings

If the Critic recommended `kill` or `defer`, stop and report the
rationale verbatim. Surface the Critic's sharp questions to the user
and ask how they want to proceed. Don't push them to override the
Critic — that's the entire point of the tool.
