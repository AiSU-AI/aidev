package doctor

import (
	"fmt"
	"os"

	"github.com/aisu-ai/aidev/internal/config"
)

// checkConfig validates that the loaded config has at least one tier and
// that every routed role has a backing provider. Most of this is already
// validated by config.Load, so this check is a belt-and-suspenders report
// rather than a gate — if the config is broken we wouldn't have gotten this
// far.
func checkConfig(cfg *config.Config) Result {
	r := Result{Name: "config"}
	if cfg == nil {
		r.Severity = FAIL
		r.Message = "config not loaded"
		r.Remediation = "check AIDEV_CONFIG env var or ./config directory"
		return r
	}
	if len(cfg.Models.Tiers) == 0 {
		r.Severity = FAIL
		r.Message = "no model tiers defined"
		r.Remediation = "add at least one tier to config/models.yaml"
		return r
	}
	if len(cfg.Models.Routing) == 0 {
		r.Severity = FAIL
		r.Message = "no agent routing defined"
		r.Remediation = "add role -> tier mappings to config/models.yaml"
		return r
	}
	if len(cfg.Principles.Principles) == 0 {
		r.Severity = WARN
		r.Message = "no principles loaded — Critic will have nothing to cite"
		r.Remediation = "add at least one principle to config/principles.yaml"
		return r
	}
	r.Severity = OK
	r.Message = "config loaded from " + cfg.ConfigDir
	r.Details = append(r.Details,
		numTiersDetail(len(cfg.Models.Tiers)),
		numRolesDetail(len(cfg.Models.Routing)),
		numPrinciplesDetail(len(cfg.Principles.Principles)),
	)
	return r
}

// checkAnthropicKey reports whether the ANTHROPIC_API_KEY env var is set.
// It is only a FAIL if the current routing actually sends some role to an
// anthropic-typed tier. If no tier uses the anthropic provider, the key
// is irrelevant and the check is a silent OK — previously this emitted a
// noisy WARN, which was misleading for users who deliberately chose the
// claude-cli path and never intended to set the env var.
func checkAnthropicKey(cfg *config.Config) Result {
	r := Result{Name: "anthropic-key"}
	needsKey := false
	for role, tierName := range cfg.Models.Routing {
		tier, ok := cfg.Models.Tiers[tierName]
		if !ok {
			continue
		}
		if tier.Provider == "anthropic" {
			needsKey = true
			r.Details = append(r.Details, "role "+role+" -> tier "+tierName+" (anthropic)")
		}
	}
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		if needsKey {
			r.Severity = FAIL
			r.Message = "ANTHROPIC_API_KEY not set, but one or more roles route to an anthropic tier"
			r.Remediation = "export ANTHROPIC_API_KEY=sk-ant-... (or swap the anthropic tiers to local providers)"
			return r
		}
		// Not set, not needed — this is fine, not a warning.
		r.Severity = OK
		r.Message = "ANTHROPIC_API_KEY not set (not needed — no tiers use the anthropic provider)"
		return r
	}
	r.Severity = OK
	r.Message = "ANTHROPIC_API_KEY present"
	if !needsKey {
		r.Details = append(r.Details, "no roles currently route to an anthropic tier")
	}
	return r
}

// small helpers keep Details strings DRY without a formatter.
func numTiersDetail(n int) string      { return plural(n, "tier", "tiers") + " defined" }
func numRolesDetail(n int) string      { return plural(n, "role", "roles") + " routed" }
func numPrinciplesDetail(n int) string { return plural(n, "principle", "principles") + " loaded" }

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}
