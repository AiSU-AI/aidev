---
description: Bulk Critic pass over every un-triaged open issue in a repo, apply verdict labels
argument-hint: [repo-path]
allowed-tools: Bash(aidev:*), Bash(gh:*), Bash(git:*), Read
---

Run a bulk Critic triage over every open issue in a target repo that
doesn't yet have an `aidev:*` label. For each such issue, runs Scout +
Critic locally via your Claude Code subscription (no Anthropic API key,
no CI, no API budget risk), then applies one of four labels based on
the verdict:

  - `aidev:approved`         — Critic recommended `build` (ready for bundling)
  - `aidev:deferred`         — Critic recommended `defer` (not the right time)
  - `aidev:killed`           — Critic recommended `kill` (bad idea)
  - `aidev:needs-refinement` — Critic returned `unclear` (issue body needs refinement)

Use this when you sit down to do backlog work and want to know which
issues deserve your time.

STEP 1 — Parse `$ARGUMENTS`. The first token (optional) is the repo
path. Default to `.` (cwd). Expand `~` to `$HOME`.

Do NOT use `!` shell execution — use the Bash tool directly.

STEP 2 — Resolve the repo owner/name from its git remote:

    gh repo view <PATH> --json owner,name --jq '.owner.login + "/" + .name'

Capture as `<owner>/<repo>`.

STEP 3 — Find un-triaged issues:

    gh issue list --repo <owner>/<repo> --state open --limit 200 \
      --json number,title,labels \
      --jq '[.[] | select([.labels[].name] | any(startswith("aidev:")) | not)]'

Filter: any issue whose label set does NOT contain any `aidev:*` entry.
If the result is empty, tell the user "no untriaged issues — backlog is
clean" and STOP.

STEP 4 — Show the user the candidate list (just titles + numbers) and
ask via `AskUserQuestion`:

  - **Proceed** (recommended) — run Critic on all candidates
  - **Preview only** — show the commands that would run, don't execute
  - **Cancel**

If `Cancel`, STOP.
If `Preview only`, print the exact `aidev` command for each candidate
and STOP.

STEP 5 — For each candidate issue (sequentially):

  a. Print a progress line: `--- [i/N] Triaging #<num>: <title> ---`

  b. Run Critic locally:

         aidev -headless -issue <num> -repo <PATH>

     This runs Scout + Critic only (no Architect without `-auto`). It
     prints the Critic report to stdout and posts the report as a
     comment to the GH issue via the existing reporter path.

  c. Parse the verdict from the last line of stdout. The report
     emits `RECOMMENDATION: build`, `RECOMMENDATION: defer`,
     `RECOMMENDATION: kill`, or `RECOMMENDATION: unclear`.

  d. Map verdict → label:

         build   → aidev:approved
         defer   → aidev:deferred
         kill    → aidev:killed
         unclear → aidev:needs-refinement

  e. Create the label if missing (idempotent):

         gh label create <label> --repo <owner>/<repo> \
           --color <rrggbb> --description "<desc>" --force

     Color suggestions: approved=0E8A16 (green), deferred=FBCA04
     (yellow), killed=B60205 (red), needs-refinement=D93F0B (orange).

  f. Apply the label + remove any stale `aidev:awaiting-decision`:

         gh issue edit <num> --repo <owner>/<repo> \
           --add-label <label> --remove-label aidev:awaiting-decision

  g. Capture the verdict + one-line Critic rationale (first bullet of
     the "Arguments FOR" or "AGAINST" section, depending on verdict)
     for the final summary.

STEP 6 — Print a final summary table. Columns: `#`, `Title`,
`Verdict`, `Label`, `Rationale (one line)`. Group by verdict so the
`aidev:approved` rows (your next-action candidates) are at the top.

STEP 7 — Tell the user what to do next:

  - If there are `aidev:approved` issues: "Run `/aidev-release
    <milestone-name> <PATH>` to bundle approved issues into a release."
  - If there are only `aidev:needs-refinement` / `aidev:deferred` /
    `aidev:killed` issues: suggest they edit issue bodies to address
    the Critic's sharp questions, then re-run `/aidev-triage`.

Safety:
  - Skip any issue whose title starts with `[follow-up #` — those
    are aidev-generated follow-up issues and already have context.
  - Cap the run at 20 issues by default; if the candidate list is
    longer, ask the user whether to process all or just the first 20.
  - If `aidev -headless` fails for any issue (exit != 0), capture the
    error into the summary row as `Verdict: ERROR` and continue with
    the next issue. Don't let one bad issue halt the batch.

Remember: this runs on YOUR machine using YOUR Claude Code
subscription. No API keys in CI, no cloud costs surprises. The total
run time is roughly `N × 60s` for N issues — typically 5-10 minutes
for a normal backlog.
