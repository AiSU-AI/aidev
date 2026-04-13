# aidev Claude Code plugin

This directory contains slash commands that integrate `aidev` into your
local Claude Code setup. Once installed, you can invoke aidev's agents
directly from inside any Claude Code session:

```
/aidev-run https://github.com/org/repo/issues/42 ~/code/repo
/aidev-doctor
/aidev-charter ~/code/repo
/aidev-test ~/code/repo
/aidev-review https://github.com/org/repo/issues/42 ~/code/repo
```

Each slash command shells out to the `aidev` binary. The binary must
be on your `PATH`.

## One-command install

```sh
aidev plugin install
```

This copies the slash command files from `plugin/aidev/commands/` into
`~/.claude/commands/` (or `$CLAUDE_CONFIG_DIR/commands/` if that env var
is set). Existing files with the same name are preserved — the
installer refuses to overwrite. Pass `--force` to overwrite.

To uninstall:

```sh
aidev plugin uninstall
```

## Manual install

If you prefer to manage Claude Code config by hand:

```sh
mkdir -p ~/.claude/commands
cp plugin/aidev/commands/*.md ~/.claude/commands/
```

Then in any Claude Code session, type `/` to see the new commands
listed alongside your existing ones.

## What the commands do

| Command | Effect |
|---|---|
| `/aidev-run <issue> <repo>` | Full headless pipeline: scout → critic → (auto) architect → (sketch 1) implementer. Writes `.aidev/proposed.patch`. |
| `/aidev-doctor` | Environment audit: config, Anthropic key, Claude CLI presence, Ollama daemon + models. |
| `/aidev-charter <repo>` | 5-question interview to produce `.aidev/charter.md` for a repo with no strong signal. |
| `/aidev-test <repo>` | Detect the project's test runner and run it. Summarises failures on non-zero exit. |
| `/aidev-review <issue> <repo>` | Boy Scout pass on `.aidev/proposed.patch`, producing blockers / suggestions / follow-up issue proposals. |

## Works with your subscription out of the box

aidev's default `config/models.yaml` routes the medium and large tiers
through `claude --print` (the Claude Code CLI non-interactive mode).
This means the Critic, Architect, Charter, Implementer, and Reviewer
all bill against your Max/Pro subscription, not against a separate
Anthropic API credit. No `ANTHROPIC_API_KEY` required.

If you prefer the direct API path for some or all tiers, edit
`config/models.yaml` in the aidev checkout and flip `provider: claude-cli`
to `provider: anthropic` — the tier system is otherwise unchanged and
you can mix providers freely.

## Troubleshooting

- `/aidev-doctor` FAIL on `claude-cli-binary`: install Claude Code
  from https://claude.ai/download and make sure `claude` is on PATH.
- `/aidev-doctor` FAIL on `anthropic-key`: you've edited
  `config/models.yaml` to use the `anthropic` provider but haven't set
  `ANTHROPIC_API_KEY`. Either set the env var or revert the config.
- Slash commands don't appear: check that files exist under
  `~/.claude/commands/aidev-*.md` and restart your Claude Code session.
