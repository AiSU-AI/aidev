---
description: Run the aidev decision pipeline (Scout/Critic/Architect) on a GitHub issue, then implement the chosen sketch natively
argument-hint: <issue-number-or-url> [repo-path]
allowed-tools: Bash(aidev:*), Bash, Read, Grep, Glob, Edit, Write, AskUserQuestion
---

You are driving `aidev`, the multi-agent decision tool, and then
implementing the chosen sketch yourself using your own native tools.

aidev is the **decision layer**: Scout (repo brief) → Critic
(adversarial build/defer/kill) → Architect (N divergent sketches).
It writes a self-contained handoff doc to
`<repo>/.aidev/architect-output.md` and exits. **You** are the
**implementation layer** — after aidev exits, you read the handoff
doc, the user picks a sketch, and you implement it with Edit/Write/
Bash inside the same Claude Code session.

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

    aidev -headless -auto -n 3 -issue <ISSUE> -repo <PATH>

Where `<ISSUE>` is the literal first token from `$ARGUMENTS` and
`<PATH>` is the literal second token (or `.` if unset). Expand `~`
to `$HOME`. Wrap the path in double quotes if it contains spaces.

aidev does NOT take a `-sketch` flag — implementation is your job
after aidev exits. `-n 3` asks the Architect for 3 divergent
sketches; the user picks one in STEP 7.

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

    aidev -headless -auto -n 3 -force-verdict build -issue <ISSUE> -repo <PATH>

This is a hard escape hatch. It logs loudly in aidev's output so
the override is always visible in the run history. Do NOT reach for
it before the two interview rounds — it bypasses the entire point
of the Critic. Use it only when you're confident the Critic is
spinning on questions the human has already resolved.

STEP 6 — Once the Critic says **build** (or the user has forced
build) and the Architect + Selector have run, BEFORE reading the
handoff doc, check the aidev stdout for the marker
`AIDEV_NEEDS_REFINEMENT=1`. If present:

  - The Selector refused to pick (all sketches disqualified, or no
    sketch met the rubric's minimum implementable score).
  - aidev posted a structured "🤔 needs refinement" comment to the
    GH issue with checklist + suggested next actions.
  - STOP. Do not implement, do not create a branch, do not file a
    PR. Tell the user "aidev needs refinement — see the GH issue
    comment" and link the issue URL.
  - This is the autonomous escape hatch: when the issue isn't ready,
    the human gets explicit hand-off instructions instead of a
    half-baked PR.

Otherwise (no refinement marker), find and READ the handoff doc.
Its location:

    $XDG_DATA_HOME/aidev/runs/<owner>-<repo>-<issue>/architect-output.md

(or `~/.local/share/aidev/runs/...` when XDG_DATA_HOME is unset).
Use the `<ISSUE>` and the repo's `<owner>/<repo>` from `gh repo view
--json owner,name --jq '.owner.login + "/" + .name'` to construct the
full path. The orchestrator also prints the path in its stdout when
it writes the file — capture it from the Bash output if you have it.

The handoff doc contains, in order:
  1. The Selector verdict (chosen sketch, score, rationale, all-scores
     table) — at the top
  2. Scout brief
  3. Critic report
  4. All N Architect sketches in full

The Selector has ALREADY chosen a sketch. Surface for the user:
  - what the Critic recommended (and whether it was overridden)
  - the Selector's chosen sketch number and title
  - the Selector's rationale (1-3 sentences from the verdict block)
  - any Critic concerns worth addressing during implementation
    (non-blocking, but worth surfacing)

If the Selector verdict shows **No sketch was chosen** (i.e.
`ChosenNumber: 0` — happens when all sketches were disqualified by a
critical-principle veto, or every sketch fell below the rubric's
MinImplementableScore), STOP. Do not implement anything. Tell the
user the Selector escalated for refinement and link the
architect-output.md path so they can read the rationale.

STEP 7 — Pre-implementation safety check. Before touching ANY file,
verify the working tree is clean:

    git -C <PATH> status --porcelain

If the output is non-empty, STOP. Tell the user "your working tree
has uncommitted changes — commit, stash, or discard them, then re-run
`/aidev-run`". Do not auto-stash; the user's in-progress work is
sacred. (No-op when the only "untracked" file is `.aidev/` artifacts
the user already gitignores.)

STEP 8 — Implement the **Selector's chosen sketch** using your
native Edit/Write/Bash tools. The Architect's sketch is the
contract — it lists files, scope, principles, and risks. The
sketch's "Approach" + "Key decisions" + "Rough scope" sections
are authoritative. Do not deviate without explicit user permission.

The Selector chose autonomously by design. Do NOT use
`AskUserQuestion` to second-guess the pick — the user opted into
autonomy by running `/aidev-run`. If they wanted a manual pick
they would have re-run aidev with `-interactive` (which the slash
command does NOT pass by default).

If the chosen sketch's `Risks` section flags a question that needs
human judgment (e.g., "verify the visual context of the contactSales
call site"), STOP before that step and ask the user via
`AskUserQuestion`. The Selector picks the SKETCH; humans still own
copy/UX/policy calls inside the implementation.

**Maintain an implementation decisions journal in memory.** As you
implement, every time you make a non-trivial choice that ISN'T in
the sketch — picking an existing helper over writing new code,
discovering the repo uses Vitest not Jest, deciding to skip an
edge case the sketch hand-waved — append a one-line bullet to the
journal. Keep entries short ("Used existing helper formatI18nKey
in src/lib/i18n/format.ts"). The journal lands in the PR body and
the final issue comment.

STEP 9 — Run the project's test suite. Detect by reading the repo:
  - `package.json` → look for `scripts.verify` / `scripts.test` /
    `scripts.lint`. Prefer `pnpm verify` if it exists.
  - `Makefile` → look for `make test` / `make check`.
  - `Cargo.toml` → `cargo test`.
  - `go.mod` → `go test ./...`.
  - Else: skip and note "no test command detected" in the PR body.

If tests fail, STOP before opening the PR. Post the failure to the
GH issue as a comment via `gh issue comment <ISSUE> --body "..."`
("aidev tried to implement Sketch N but tests failed — see the
attached output"). The user reviews and decides whether to override.
Do not commit or push when tests fail.

STEP 10 — Open the PR. Resolve the base branch in this priority:

  1. `<PATH>/.aidev/aidev.yaml` → `pr.base_branch` if present
     (read with `cat | yq` or grep — keep it simple)
  2. `preview` if it exists on origin (`git ls-remote --heads
     origin preview`)
  3. The repo's default branch (`gh repo view --json
     defaultBranchRef --jq '.defaultBranchRef.name'`)

Branch name: `aidev/issue-<ISSUE>-<slug>` where `<slug>` is the
issue title lowercased, non-alphanum collapsed to `-`, capped at
40 chars. If the branch already exists locally OR on origin,
append `-2`, `-3`, etc. until unique.

Commit message format (per the user's global rules in
~/.claude/CLAUDE.md): single-line `<type>: <description>`, no body,
no trailers, no `Co-Authored-By`. Type from the chosen sketch:

  - bug-class issues → `fix`
  - new feature → `feat`
  - cleanup / consolidation → `refactor`
  - docs only → `docs`

PR title matches the commit message verbatim. PR body uses HEREDOC
(per user's global rules):

```
## Summary
<one-line restatement of the issue>

Implements aidev Sketch <N>: <title>

## Selector verdict
<paste the rationale from the architect-output.md verdict block>

**Score:** <X.X> · **Rubric version:** <v> · **Tie-breaker:** <yes|no>

## Implementation decisions
<bulleted journal you maintained in STEP 8>

## Test plan
- [ ] <list from the chosen sketch's "Rough scope" section>
- [ ] <project test command> passes

Closes #<ISSUE>

🤖 Generated with [Claude Code](https://claude.com/claude-code)
```

Then:

  1. `git -C <PATH> checkout -b <branch-name>`
  2. `git -C <PATH> add <files-touched>` (specific files, not `-A`)
  3. `git -C <PATH> commit -m "<type>: <description>"`
  4. `git -C <PATH> push -u origin <branch-name>`
  5. `gh pr create --base <base-branch> --head <branch-name>
     --title "<title>" --body "$(cat <<'EOF'
     ...
     EOF
     )"`

NEVER force-push. Always create a fresh branch. If anything in this
sequence fails (push rejected, gh auth missing, etc.), surface the
error to the user and STOP.

STEP 11 — Post the PR URL back to the original GH issue:

    gh issue comment <ISSUE> --body "$(cat <<'EOF'
    ## ✅ aidev opened #<PRNUM> for review

    Implementation of Sketch <N> from the Selector's pick. Tests
    passed locally before push.

    ## Implementation decisions

    <same journal that's in the PR body>

    Branch: \`<branch-name>\` → \`<base-branch>\`

    🤖 aidev autonomous run
    EOF
    )"

Then summarise to the user: "Opened PR #<PRNUM>. Audit trail in
issue #<ISSUE>." Done.

Never push the user to override a `kill` verdict; that is the entire
point of the tool. A `defer` or `unclear` verdict, however, is an
invitation to dialogue — which is exactly what STEPS 4 and 5 are for.
