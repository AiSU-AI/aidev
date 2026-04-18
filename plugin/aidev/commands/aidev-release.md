---
description: Show + confirm a release bundle for a given GitHub milestone
argument-hint: <milestone-name> [repo-path]
allowed-tools: Bash(gh:*), Bash(aidev:*), Read
---

Assemble (or inspect) a release bundle around a GitHub milestone. Takes
you from "these issues are `aidev:approved`" to "these issues are in
milestone v0.4.0 and ready for `/aidev-implement-bundle`."

Workflow this command supports:

  1. You triage new issues with `/aidev-triage` — those with Critic
     verdict `build` get the `aidev:approved` label.
  2. You run `/aidev-release v0.4.0 <repo>` — this command shows you
     (a) what's already in the milestone, and (b) approved-but-unassigned
     candidates. You pick which candidates to add.
  3. When the milestone looks right, you run
     `/aidev-implement-bundle v0.4.0 <repo>` to ship it.

STEP 1 — Parse `$ARGUMENTS`. First token (required) is the milestone
name. Second token (optional) is the repo path; default `.` (cwd).

If `$ARGUMENTS` is missing the milestone name, STOP and ask the user
for one.

STEP 2 — Resolve repo owner/name:

    gh repo view <PATH> --json owner,name --jq '.owner.login + "/" + .name'

STEP 3 — Ensure the milestone exists. Look it up:

    gh api repos/<owner>/<repo>/milestones?state=open \
      --jq '.[] | select(.title == "<name>") | {number, title, url, due_on}'

If not found, ask the user via `AskUserQuestion`:

  - **Create milestone `<name>` now** — `gh api repos/<owner>/<repo>/milestones -X POST -f title=<name>`
  - **Use a different name** — prompt for a new name
  - **Cancel**

STEP 4 — List what's currently in the milestone:

    gh issue list --repo <owner>/<repo> --milestone "<name>" --state open \
      --json number,title,labels,body \
      --jq '.'

Show the user each one as: `#<num>  [<verdict>]  <title>` where
`<verdict>` is inferred from the `aidev:*` labels (`approved` →
`approved`, `needs-refinement` → `⚠ needs refinement`, etc).

STEP 5 — List candidate issues (approved, no milestone assigned):

    gh issue list --repo <owner>/<repo> --state open --label aidev:approved \
      --no-milestone --json number,title,body \
      --jq '.'

Note: `--no-milestone` filter; only the issues that have
`aidev:approved` AND are currently unassigned to any milestone.

STEP 6 — Show the user the two lists in this order:

    Milestone <name> (N issues):
      - #<num>  <title>                    [verdict]
      ...

    Approved candidates (M available, no milestone yet):
      - #<num>  <title>                    [Critic one-line]
      ...

If there are zero candidates AND zero in-milestone issues, say
"Milestone `<name>` is empty; run `/aidev-triage` first to approve
issues." and STOP.

STEP 7 — Ask the user what to do via `AskUserQuestion`:

  - **Add all candidates to the milestone** — bundle everything approved
  - **Pick specific candidates** — interactive pick (next step)
  - **Remove an issue from the milestone** — unbundle one
  - **Show details of a candidate** — paste the Critic rationale for a specific issue
  - **Done — bundle looks good** — finalize and show next steps
  - **Cancel**

STEP 8 — Handle the user's choice:

  - `Add all candidates`: for each candidate issue, run:

        gh issue edit <num> --repo <owner>/<repo> \
          --milestone "<name>" \
          --remove-label aidev:approved \
          --add-label aidev:bundled

    Then loop back to STEP 4 to show the updated state.

  - `Pick specific candidates`: use `AskUserQuestion` with each
    candidate as an option (up to 4 per question; for larger lists,
    iterate in pages of 4). For each `Yes` answer, apply the same
    milestone edit + label swap as above.

  - `Remove an issue`: ask for the issue number, then:

        gh issue edit <num> --repo <owner>/<repo> \
          --milestone "" \
          --remove-label aidev:bundled \
          --add-label aidev:approved

  - `Show details of a candidate`: ask for the number, then print:

        gh issue view <num> --repo <owner>/<repo> --comments

  - `Done`: jump to STEP 9.

  - `Cancel`: STOP.

STEP 9 — Finalize. Print a summary of the milestone:

    Release candidate: <name>
      <N> issues bundled:
        #<num>  <title>
        ...

      Ready to ship? Run:
        /aidev-implement-bundle <name> <PATH>

      That will run the full autonomous pipeline on each issue in
      sequence, opening one PR per issue against origin/preview.

STEP 10 — Optional: post a tracking comment to a pinned meta-issue
(if one exists with label `aidev:release-tracker` and title matching
`Release <name>`) so the bundle state is visible publicly. Skip if no
such issue exists; don't auto-create one.

Safety:
  - Never delete a milestone or close issues. This command only
    assigns / unassigns.
  - Label swaps are idempotent; running this command multiple times
    converges on the same state.
  - If `<name>` contains shell-special chars (quotes, `$`), wrap in
    single quotes when passing to `gh`.

This command is interactive by design — bundling release scope is a
human-in-the-loop decision. aidev shows you the material; you pick.
