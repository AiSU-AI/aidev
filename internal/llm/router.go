package llm

import (
	"fmt"
	"time"

	"github.com/aisu-ai/aidev/internal/config"
)

// Router maps agent roles to Provider instances via the tier indirection
// defined in models.yaml. Construct one per process; Router is safe to share
// across goroutines because Provider implementations are stateless above the
// HTTP client.
type Router struct {
	tiers    map[string]Provider
	routing  map[string]string // role name -> tier name
	recorder *Recorder
}

// NewRouter builds a Router from a loaded config. It instantiates one
// Provider per tier; unknown provider types in models.yaml become a hard
// error at startup rather than at first use. Every tier provider is wrapped
// with the router's shared telemetry recorder so per-gate token counts and
// latency are captured automatically for the headless report.
func NewRouter(cfg *config.Config) (*Router, error) {
	r := &Router{
		tiers:    make(map[string]Provider, len(cfg.Models.Tiers)),
		routing:  cfg.Models.Routing,
		recorder: NewRecorder(),
	}
	for name, t := range cfg.Models.Tiers {
		p, err := buildProvider(t, r.recorder)
		if err != nil {
			return nil, fmt.Errorf("tier %q: %w", name, err)
		}
		r.tiers[name] = p
	}
	return r, nil
}

// Recorder returns the router's telemetry recorder. Callers use this to
// render a per-gate summary after a run completes.
func (r *Router) Recorder() *Recorder { return r.recorder }

func buildProvider(t config.Tier, recorder *Recorder) (Provider, error) {
	var base Provider
	switch t.Provider {
	case "ollama":
		base = NewOllamaWithStreaming(t.Endpoint, t.Model, t.MaxTokens, t.Temperature)
	case "anthropic":
		base = NewClaude(t.Model, t.MaxTokens, t.Temperature)
	case "claude-cli":
		base = NewClaudeCLI(t.Model, t.MaxTokens, t.Temperature)
	default:
		return nil, fmt.Errorf("unknown provider %q", t.Provider)
	}

	// Apply middleware based on provider type
	var middlewares []func(Provider) Provider

	// Add retry logic for all providers
	retryConfig := DefaultRetryConfig()
	if t.Provider == "ollama" {
		// Ollama might need more retries due to local resource constraints
		retryConfig.MaxAttempts = 5
		retryConfig.BaseDelay = 2 * time.Second
	}
	middlewares = append(middlewares, WithRetry(retryConfig))

	// Add circuit breaker for cloud providers to prevent cascading failures
	if t.Provider == "anthropic" || t.Provider == "claude-cli" {
		middlewares = append(middlewares, WithCircuitBreaker(3, 60*time.Second))
	}

	// Telemetry goes OUTSIDE retry and circuit breaker so each recorded
	// "call" represents one logical request from the caller's perspective.
	// Retries are absorbed into the elapsed time on that single record.
	if recorder != nil {
		middlewares = append(middlewares, WithTelemetry(recorder))
	}

	return ProviderWithMiddleware(base, middlewares...), nil
}

// For returns the Provider assigned to the given role. Unknown roles return
// an error so callers are forced to update routing when a new agent is added.
//
// RoleCoordinator is the one exception: because it was added after users
// already had models.yaml files on disk, an unrouted Coordinator role
// transparently falls back to RoleCritic's tier (both are judgment tasks
// that want the large tier). This keeps older configs working without a
// config migration step. The fallback is logged nowhere because it's the
// correct default; users who want a different tier for the Coordinator
// specifically add 'coordinator: <tier>' to their routing block and the
// fallback never fires.
func (r *Router) For(role Role) (Provider, error) {
	tier, ok := r.routing[string(role)]
	if !ok {
		if role == RoleCoordinator {
			if fallback, haveCritic := r.routing[string(RoleCritic)]; haveCritic {
				tier = fallback
				ok = true
			}
		}
		if !ok {
			return nil, fmt.Errorf("router: no tier configured for role %q", role)
		}
	}
	p, ok := r.tiers[tier]
	if !ok {
		return nil, fmt.Errorf("router: tier %q not built", tier)
	}
	return p, nil
}

// Tiers returns the tier name -> Provider map, used by the TUI status bar
// to display provider health.
func (r *Router) Tiers() map[string]Provider {
	out := make(map[string]Provider, len(r.tiers))
	for k, v := range r.tiers {
		out[k] = v
	}
	return out
}
