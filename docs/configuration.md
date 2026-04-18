---
title: Configuration
---

# Configuration

aidev is driven by four config surfaces, each with a sensible default:

| File | Scope | Purpose |
|---|---|---|
| `~/.config/aidev/models.yaml` | global | Which LLM (tier) runs which agent |
| `~/.config/aidev/principles.yaml` | global | The Critic's argument basis |
| `<repo>/.aidev/principles.yaml` | per-repo | Extra principles merged on top of the global set |
| `<repo>/.aidev/charter.md` | per-repo | 5-question product charter when the Critic has nothing to anchor against |
| `<repo>/.aidev/aidev.yaml` | per-repo | PR base branch + review loop knobs (max rounds, protected paths, etc.) |
| `<repo>/.aidev/sketch-rubric.yaml` | per-repo | Selector's scoring weights + `min_implementable_score` override |

## `~/.config/aidev/models.yaml`

Three tiers (`small`, `medium`, `large`) mapped to providers (`ollama`, `anthropic`, `claude-cli`), plus a role-to-tier routing table. The shipped defaults:

```yaml
tiers:
  small:  { provider: ollama,     model: qwen2.5-coder:7b,  ... }
  medium: { provider: claude-cli, model: "" }
  large:  { provider: claude-cli, model: "" }
routing:
  scout: small
  critic: large
  architect: large
  charter: large
  clarifier: large
  selector: large
  triage: large
  coordinator: large
  implementer: medium
  reviewer: medium
  tester: small
```

Want the Critic on the direct Anthropic API with a specific model?

```yaml
large: { provider: anthropic, model: claude-opus-4-6 }
```

…and export `ANTHROPIC_API_KEY`. Want Scout on Claude instead of local Ollama? Change `scout: small` to `scout: large`.

Unmapped roles fall back gracefully: `selector` and `triage` fall back to `critic`'s tier when not explicitly routed, so older `models.yaml` files keep working without a migration.

## `~/.config/aidev/principles.yaml`

The living charter the Critic uses when arguing whether a feature should ship. Ships with:

- Should this exist?
- Boy Scout Rule
- DRY (Rule of Three)
- YAGNI
- Well-Architected
- Single Responsibility
- Fail Loudly at Boundaries
- Tests Describe Intent
- Reversibility

Add your own; the Critic cites them by name.

## `<repo>/.aidev/principles.yaml` (per-repo)

Merged on top of the global set — useful when an organisation has a standards repo that downstream projects inherit from. Repo-local principles take precedence when names collide.

## `<repo>/.aidev/charter.md`

When a target repo has no strong signal — no README, no `CLAUDE.md`, no `ARCHITECTURE.md` — the Critic has nothing to anchor against. Run:

```sh
aidev charter -repo <path>
```

…and sit through a 5-question interview. The output lands in `<repo>/.aidev/charter.md`. The Scout automatically absorbs it on every subsequent run and the Critic cites it when pushing back on proposals.

## `<repo>/.aidev/clarifier.md` (session-scoped)

After the Critic flags ambiguities in a proposal, `aidev clarify -issue ... -repo ...` runs a dependency-aware question graph. Independent questions batch; chained questions serialise. Answers write to `<runDir>/clarifier.md` and the next Critic run absorbs them as authoritative ground truth (the Critic is prompted never to re-ask already-answered questions).

## `<repo>/.aidev/aidev.yaml` (per-repo behavior knobs)

Shape the slash command reads:

```yaml
pr:
  base_branch: preview    # override the default (preview if exists, else repo default)
review:
  max_rounds: 3           # cap on review-loop iterations before forcing escalate
  wait_for_ci: true
  ci_timeout_minutes: 30
  protected_paths:        # fixes touching these paths auto-escalate
    - "**/migrations/**"
    - "*.sql"
    - "**/.github/workflows/**"
    - "**/secrets/**"
    - "*.env"
    - "*.env.*"
coordinator:
  comment_threshold: warn   # info | warn | concern — minimum severity that promotes to PR comment
```

All keys are optional; absent keys fall back to the defaults shown above.

## `<repo>/.aidev/sketch-rubric.yaml` (per-repo Selector override)

Overrides the shipped `defaultRubricYAML` with repo-specific weights:

```yaml
version: "1.0"
weights:
  aligned: 1.0
  tension: -1.0
  violation: -2.0     # non-critical principles only
  risk: -0.5
  risk_cap: -3.0      # cap on the accumulated risk penalty
critical_principles:
  - "Single Responsibility"
  - "DRY (Rule of Three)"
  - "Reversibility"
issue_type_overrides:
  bug:
    scope_files_penalty: 0.1     # per file beyond threshold
    scope_files_threshold: 5
  feature:
    scope_files_penalty: 0.0
    scope_files_threshold: 0
tie_breaker_epsilon: 1.0
min_implementable_score: 0.0
```

Critical-principle violations produce a hard veto (score = -∞). Non-critical violations add weighted penalty. `min_implementable_score: 0.0` means "at least net-neutral alignment required" — was tightened to `1.0` earlier in v0.3.0 development; relaxed because the Critic already gates "should we build this?" and over-tight rubric floors caused false-positive refinement loops on real issues.

## See also

- [Architecture](architecture.md) — how the config surfaces flow through the pipeline
- [Review loop](review-loop.md) — deep dive on `review.*` keys and the Triage safety envelope
