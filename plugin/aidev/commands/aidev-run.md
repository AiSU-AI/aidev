---
description: Run the full aidev agent pipeline on a GitHub issue
argument-hint: <issue-number-or-url> [repo-path]
allowed-tools: Bash(aidev:*), Read, Grep, Glob, Write, AskUserQuestion
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

STEP 3 — Read the Critic verdict from the output.

  - **build** → proceed to STEP 6.
  - **kill** → STOP. The Critic killed the proposal on principle;
    looping is not the answer. Report the rationale verbatim and let
    the user decide whether to override out-of-band.
  - **unclear** or **defer** → do NOT stop. Go to STEP 4.

STEP 4 (self-research) — Before bothering the user, try to answer
the Critic's sharp questions yourself by reading the codebase. You
have `Read`, `Grep`, and `Glob`. Most Critic questions fall into two
buckets:

  - **Evidence questions** — "are there other consumers of X?",
    "is this a button label or body copy?", "does the existing
    namespace already include Y?", "what is the blast radius of
    deleting Z?". These are answerable from the repo. Run the greps
    and reads YOURSELF and produce concrete findings: file paths,
    line numbers, snippets. The user should not be asked things you
    can verify in a few seconds.

  - **Judgment questions** — "should we ship now or split into a
    follow-up?", "do non-English locales need native-reviewer
    signoff?", "is this the right priority?". These need the human.
    Save them for STEP 5.

When you classify a Critic question as "evidence", run the actual
investigation (Grep across the WHOLE repo, not just one subdir;
Read the actual call sites; Glob the file types the Critic is
worried about including markdown, MDX, JSON, generated types,
storybooks, tests, analytics events). Capture the literal output
you got — file paths and line numbers count, hand-waving doesn't.

STEP 5 (judgment interview) — For the questions that survived STEP 4
(human-judgment questions only), ask the user one at a time using
the `AskUserQuestion` tool. Keep the question text short and close
to the Critic's wording. If a question is multi-part, split it.
Give the user an "escape" option ("let me think / stop pipeline")
on every question so they're never forced into an answer.

After collecting answers, WRITE the full clarifier session to
`<repo>/.aidev/clarifier.md` using the Write tool. Include BOTH
the evidence findings from STEP 4 AND the user's answers from
STEP 5. The on-disk format aidev's Scout reads:

    # Clarifier session

    _Recorded by Claude Code._

    ## Wave 1

    ### q1 — <question>

    **Answer:** <evidence finding from STEP 4 OR user answer from STEP 5>

    ### q2 — <question>

    **Answer:** <...>

    ...

Use `q1`, `q2`, … as IDs. Mark each Answer as either
`(evidence — Claude Code)` or `(user)` so the audit trail makes
clear which findings came from research vs. from the developer.

Then re-invoke the SAME aidev command from STEP 2. aidev's Critic
will pick up `.aidev/clarifier.md` and re-decide with both your
research and the user's judgment as authoritative input.

Do this STEP 4 → STEP 5 → re-invoke loop AT MOST TWICE.

STEP 5b (escape hatch — only after 2 full interview rounds) — If
after two rounds the Critic is STILL hedging and the user has
genuinely answered every question that requires human judgment,
ask the user ONE final question via `AskUserQuestion`:

  > "The Critic is still hedging despite the answers we collected.
  > Would you like to force aidev to proceed to Architect + Implementer
  > anyway? The Critic's report stays in the audit trail."

Options: "Force build" / "Stop here". On "Force build", re-invoke
aidev one more time with the additional flag:

    aidev -headless -auto -sketch 1 -force-verdict build -issue <ISSUE> -repo <PATH>

This is a hard escape hatch. It logs loudly in aidev's output so
the override is always visible in the run history. Do NOT reach for
it before the two interview rounds — it bypasses the entire point
of the Critic. Use it only when you're confident the Critic is
spinning on questions the human has already resolved.

STEP 6 — Once the Critic says **build** (or the user has forced
build) and the Architect + Implementer have run, summarise the
final state for the user:
  - what the Critic recommended (and whether it was overridden)
  - how many sketches the Architect produced
  - whether the Implementer wrote a patch at
    `<repo>/.aidev/proposed.patch`
  - any doctor warnings

Never push the user to override a `kill` verdict; that is the entire
point of the tool. A `defer` or `unclear` verdict, however, is an
invitation to dialogue — which is exactly what STEPS 4 and 5 are for.
