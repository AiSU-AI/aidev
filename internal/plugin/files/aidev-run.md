---
description: Run the full aidev agent pipeline on a GitHub issue
argument-hint: <issue-number-or-url> [repo-path]
allowed-tools: Bash(aidev:*), Write
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

STEP 3 — Read the Critic verdict from the output.

  - **build** → proceed to STEP 5.
  - **kill** → STOP. The Critic killed the proposal on principle;
    looping is not the answer. Report the rationale verbatim and let
    the user decide whether to override out-of-band.
  - **unclear** or **defer** → do NOT stop. Go to STEP 4.

STEP 4 (interview) — The Critic has sharp questions that need human
input before a build verdict is reachable. Your job is to interview
the user, persist their answers where aidev can see them, and re-run
the pipeline.

Sub-step 4a: Extract the Critic's sharp questions from the report.
They're in a section titled roughly "Sharp questions" or numbered
1/2/3 under the FOR/AGAINST arguments. There are usually 2–3 of
them.

Sub-step 4b: Ask the user each question, one at a time, using the
`AskUserQuestion` tool. Keep the question text short and close to
the Critic's own wording. If a question is multi-part, split it.
Give the user an "escape" option ("let me think / stop pipeline")
on every question so they're never forced into an answer.

Sub-step 4c: Once you have answers, WRITE them to
`<repo>/.aidev/clarifier.md` using the Write tool with this exact
shape — aidev's Scout already knows how to pick up this file on the
next run:

    # Clarifier session

    _Recorded by Claude Code._

    ## Wave 1

    ### q1 — <the first question you asked>

    **Answer:** <the user's answer>

    ### q2 — <the second question>

    **Answer:** <the user's answer>

    ...

Use `q1`, `q2`, … as IDs. If a question depended on an earlier
answer, put it in a `## Wave 2` section instead.

Sub-step 4d: Re-invoke the SAME aidev command from STEP 2. The new
run will pick up `.aidev/clarifier.md` automatically via the Scout
and the Critic will re-decide with the human's answers as
authoritative input.

After the second run, go back to STEP 3. Do this at most TWICE
(i.e. up to two interview rounds). If the Critic still refuses
after two rounds, stop and report the latest verdict verbatim —
the Critic is telling you something real.

STEP 5 — Once the Critic says **build** and the Architect +
Implementer have run, summarise the final state for the user:
  - what the Critic recommended
  - how many sketches the Architect produced
  - whether the Implementer wrote a patch at
    `<repo>/.aidev/proposed.patch`
  - any doctor warnings

Never push the user to override a `kill` verdict; that is the entire
point of the tool. A `defer` or `unclear` verdict, however, is an
invitation to dialogue — which is exactly what STEP 4 is for.
