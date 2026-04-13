---
description: Run the aidev Charter interview against a target repo
argument-hint: <repo-path>
allowed-tools: Bash(aidev:*), Write
---

Run the aidev Charter interview for the repository at `$ARGUMENTS`.

The Charter interview collects five answers and then asks the
large-tier model to synthesise them into a structured product charter
at `<repo>/.aidev/charter.md`. The aidev binary has an interactive
mode (stdin prompts) for terminal use and a non-interactive mode
(`--answers-file <json>`) for programmatic use. Because Claude Code
slash commands have no attached TTY, we ALWAYS use the non-interactive
mode here — collecting the five answers from the user IN THIS CHAT,
writing them to a temp JSON file, then invoking aidev to do the
synthesis and file write.

STEP 1 — Validate the argument.
If `$ARGUMENTS` is empty, ask the user which repository they want to
create a charter for and STOP. Accept either an absolute path or one
starting with `~` (expand `~` to `$HOME` before passing it to bash).
Do not default to the current directory — the charter is
repo-specific and the wrong directory writes to the wrong place.

STEP 2 — Ask the five questions in this chat, one at a time, in
order. Wait for the user's answer to each before moving to the next.

  1. In one sentence, what does this product do?
  2. Who uses it? (Users or audiences, in plain language.)
  3. What's the single most important constraint?
     (correctness / latency / cost / security / compliance / something else)
  4. What is explicitly out of scope — things this product should
     NOT do, even if technically possible?
  5. (Optional) Any engineering principles that override or extend
     the defaults in `config/principles.yaml`? Leave blank to skip.

Be patient. Don't paraphrase the user's answers back or ask
follow-ups for clarification unless they wrote something
contradictory — the Charter agent has its own prompt and will
synthesise the answers into a tight document.

STEP 3 — Write the answers to a temp JSON file. Use the Write tool
(NOT a bash heredoc) to create the file at
`/tmp/aidev-charter-answers-<random>.json` with this EXACT shape
(string values for every key; overrides may be an empty string):

    {
      "purpose": "<answer 1>",
      "users": "<answer 2>",
      "constraint": "<answer 3>",
      "out_of_scope": "<answer 4>",
      "overrides": "<answer 5 or empty string>"
    }

STEP 4 — Run aidev with the non-interactive flag. Use the Bash tool
with this command (substituting the real paths):

    aidev charter -repo "<the-path-from-$ARGUMENTS>" --answers-file "/tmp/aidev-charter-answers-<random>.json"

Do NOT use the `!` shell-execution prefix for this — we need to wait
for Step 3 to complete before running Step 4, and `!` fires before
the chat body is processed. Use the Bash tool call.

STEP 5 — After the command completes, read the resulting
`<repo>/.aidev/charter.md` file and summarise its Purpose section for
the user so they can quickly verify it captured their intent. Also
remind them the charter is committable — they should `git add` it if
they want the team to see it and they should `aidev clarify` in the
future when the Critic flags ambiguity.

STEP 6 — Clean up the temp answers file.
