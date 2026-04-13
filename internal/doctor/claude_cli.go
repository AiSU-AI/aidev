package doctor

import (
	"os/exec"

	"github.com/aisu-ai/aidev/internal/config"
)

// checkClaudeCLI emits a result when any tier in the routing uses the
// `claude-cli` provider. It checks that the `claude` binary is on PATH;
// full authentication status is not verified here because probing it
// would cost a subscription call on every startup. Users who have the
// binary installed but aren't logged in will see a clear error at the
// first real agent call instead.
func checkClaudeCLI(cfg *config.Config) Result {
	r := Result{Name: "claude-cli-binary"}

	needed := false
	for _, tier := range cfg.Models.Tiers {
		if tier.Provider == "claude-cli" {
			needed = true
			break
		}
	}
	if !needed {
		r.Severity = OK
		r.Message = "no tiers use the claude-cli provider — skipping"
		return r
	}

	path, err := exec.LookPath("claude")
	if err != nil {
		r.Severity = FAIL
		r.Message = "`claude` CLI not found on PATH but one or more tiers use the claude-cli provider"
		r.Remediation = "install Claude Code from https://claude.ai/download, or edit config/models.yaml to route those tiers to 'anthropic' with an ANTHROPIC_API_KEY"
		return r
	}

	r.Severity = OK
	r.Message = "claude CLI at " + path
	r.Details = append(r.Details, "run `claude /login` if you haven't yet — agent calls will surface auth errors on first use otherwise")
	return r
}
