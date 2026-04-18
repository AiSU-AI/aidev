---
title: Install
---

# Install

## Quick install

Downloads the latest pre-built binary from the GitHub release, writes default config, installs Claude Code slash commands, runs a smoke test. No Go required:

```sh
curl -fsSL https://raw.githubusercontent.com/AiSU-AI/aidev/main/install.sh | bash -s -- --from-release
```

## From source

Needs Go 1.24+ on your `PATH`. Use this if you want to track `main` or contribute:

```sh
gh repo clone AiSU-AI/aidev ~/code/personal/aidev    # or: git clone git@github.com:AiSU-AI/aidev.git ~/code/personal/aidev
cd ~/code/personal/aidev
./install.sh
```

## What the installer does

1. Builds or downloads the binary.
2. Copies it to `~/.local/bin/aidev` (or `~/bin`, `/usr/local/bin` — first writable dir on your `PATH`).
3. Writes default config to `~/.config/aidev/{models,principles}.yaml`.
4. Installs Claude Code slash commands to `~/.claude/commands/aidev-*.md`.
5. Runs `aidev doctor` to smoke-test the environment.
6. Prints next steps.

If the install dir isn't already on your `PATH`, the script prints the exact `export PATH=...` line to add to `~/.zshrc` or `~/.bashrc`.

## Installer flags

```sh
./install.sh --bin ~/bin          # override target bin directory
./install.sh --from-source        # force build mode (default when Go is present)
./install.sh --from-release       # download from a GitHub release
./install.sh --version v0.3.0     # pin a specific release tag (release mode only)
./install.sh --force              # overwrite existing binary / config / plugin files
./install.sh --no-doctor          # skip the post-install smoke test
./install.sh --no-prereqs         # skip the 'suggest installing ollama' check
```

## Requirements

- **macOS or Linux.** Windows is not supported yet — the TUI has POSIX-isms.
- **Go 1.24+** — only for from-source builds. Install from [go.dev/dl](https://go.dev/dl/).
- **[Claude Code](https://claude.ai/download)** — installed and logged in (`claude /login`). aidev's medium/large tiers route through `claude --print`, so agent calls bill against your Max/Pro subscription. No `ANTHROPIC_API_KEY` required for the default config.
- **[Ollama](https://ollama.com)** — for the small tier (Scout, Tester failure summary). Optional; you can route the small tier through the Claude CLI too by editing `config/models.yaml`, at the cost of subscription tokens.
- **`GITHUB_TOKEN`** in your environment for reading private issues and posting the audit trail (`Issues: Read and write` scope). Auto-resolved from `gh auth token` if you've run `gh auth login`. Also used for HTTPS-based `git push`.

Run `aidev doctor` at any time to verify the environment. If Ollama is installed but not running, `doctor` will spawn `ollama serve` in the background automatically.

## Upgrading

For binary installs:

```sh
curl -fsSL https://raw.githubusercontent.com/AiSU-AI/aidev/main/install.sh | bash -s -- --from-release --force
```

For source installs, `git pull` then re-run `./install.sh --force`.

Self-updater (`aidev update`) is on the [roadmap](roadmap.md) but not yet implemented.

## See also

- [Usage](usage.md) — how to drive aidev once it's installed
- [Configuration](configuration.md) — customize models, principles, rubric
- [Troubleshooting](troubleshooting.md) — when `aidev doctor` reports issues
