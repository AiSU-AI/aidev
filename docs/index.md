---
title: aidev
layout: default
---

# aidev

A local multi-agent coding pipeline for **Claude Code** that **argues with you before it writes code**, then ships the approved work autonomously — branch cut, implementation, tests, PR, autonomous review loop — all without line-by-line human intervention.

Built in Go. Works with your Claude Max/Pro subscription — no API key required.

## Quick install

```sh
curl -fsSL https://raw.githubusercontent.com/AiSU-AI/aidev/main/install.sh | bash -s -- --from-release
```

See [Install](install.md) for the from-source path, installer flags, and full prereqs.

## First run

From inside any Claude Code session in the target repo:

```
/aidev-run 42 .
```

The first arg is an issue number or URL; the second is the local repo path. aidev runs the full autonomous pipeline — Scout → Critic → Architect → Selector → implementation → tests → PR → review loop — and lands a mergeable PR in your tree.

## Docs

- **[Install](install.md)** — prereqs, install modes, flags, updating
- **[Usage](usage.md)** — slash commands, CLI, examples, headless/CI mode
- **[Architecture](architecture.md)** — the agent pipeline, the two load-bearing insights, extensibility
- **[Review loop](review-loop.md)** — the autonomous fix/rebut/defer/escalate cycle, the safety envelope, the hooks
- **[Configuration](configuration.md)** — models.yaml, principles, charter, clarifier, sketch rubric
- **[Troubleshooting](troubleshooting.md)** — when `aidev doctor` complains
- **[Roadmap](roadmap.md)** — shipped + planned

## Why this shape

Several tools already turn issues into patches (aider, plandex, opencode, goose, Claude Code itself). **None of them argue with you first.** If the Critic decides the feature shouldn't exist, aidev tells you that and stops — it doesn't sheepishly try to implement a thing it thinks is a bad idea. That's the differentiator.

The rest of the pipeline is built around the same principle: every decision has a gate that can stop it, every action has a safety envelope (Sketch-grounded rebuttals, protected paths, cycle protection), and every outcome lands in the GitHub issue thread so the audit trail survives the session.

## Source

- Repo: [github.com/AiSU-AI/aidev](https://github.com/AiSU-AI/aidev)
- License: [MIT](https://github.com/AiSU-AI/aidev/blob/main/LICENSE)
- Issues: [github.com/AiSU-AI/aidev/issues](https://github.com/AiSU-AI/aidev/issues)
