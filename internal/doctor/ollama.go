package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/aisu-ai/aidev/internal/config"
)

// checkOllama emits one or more results depending on what it finds:
//
//  1. "ollama-binary" — is the `ollama` CLI installed
//  2. "ollama-daemon" — is the HTTP API reachable (auto-spawns if Opts.AutoSpawnOllama and interactive)
//  3. "ollama-models" — are the models required by the current routing actually pulled
//     (offers interactive fix if Opts.Interactive)
//
// Each check is only emitted if prior checks made it meaningful. If the
// binary isn't installed we don't bother checking the daemon, etc.
func checkOllama(ctx context.Context, cfg *config.Config, opts Options) []Result {
	required := requiredOllamaModels(cfg)
	if len(required) == 0 {
		// No roles route to ollama; nothing to check.
		return []Result{{
			Name:     "ollama",
			Severity: OK,
			Message:  "no roles route to an ollama tier — skipping ollama checks",
		}}
	}

	results := make([]Result, 0, 3)

	// 1. binary
	binResult := Result{Name: "ollama-binary"}
	path, err := exec.LookPath("ollama")
	if err != nil {
		binResult.Severity = FAIL
		binResult.Message = "`ollama` CLI not found on PATH"
		binResult.Remediation = "install from https://ollama.com or swap the small/medium tiers to a non-ollama provider"
		results = append(results, binResult)
		return results
	}
	binResult.Severity = OK
	binResult.Message = "ollama binary at " + path
	results = append(results, binResult)

	// 2. daemon
	daemonResult := Result{Name: "ollama-daemon"}
	endpoint := firstOllamaEndpoint(cfg)
	fmt.Fprintf(opts.Out, "checking ollama daemon at %s...\n", endpoint)
	if pingOllama(ctx, endpoint) {
		daemonResult.Severity = OK
		daemonResult.Message = "daemon reachable at " + endpoint
	} else if opts.AutoSpawnOllama {
		// Note: auto-spawn is gated only on opts.AutoSpawnOllama, not
		// opts.Interactive. Spawning `ollama serve` is idempotent and
		// the daemon persists beyond aidev's lifetime (we don't own
		// it), so it is safe to do in non-interactive contexts like
		// `aidev doctor`. Interactivity only gates user PROMPTS —
		// pulling a model or swapping to an alternative.
		fmt.Fprintf(opts.Out, "ollama daemon not reachable. spawning `ollama serve` in the background...\n")
		spawnErr := spawnOllama()
		if spawnErr != nil {
			daemonResult.Severity = FAIL
			daemonResult.Message = "failed to auto-spawn ollama: " + spawnErr.Error()
			daemonResult.Remediation = "start it manually: `ollama serve` in another terminal"
			results = append(results, daemonResult)
			return results
		}
		// Wait up to 15s for the daemon to come up.
		if waitForOllama(ctx, endpoint, 15*time.Second) {
			daemonResult.Severity = OK
			daemonResult.Message = "daemon spawned and reachable at " + endpoint
		} else {
			daemonResult.Severity = FAIL
			daemonResult.Message = "spawned ollama but it did not respond within 15s"
			daemonResult.Remediation = "start it manually and investigate: `ollama serve`"
			results = append(results, daemonResult)
			return results
		}
	} else {
		daemonResult.Severity = FAIL
		daemonResult.Message = "daemon not reachable at " + endpoint
		daemonResult.Remediation = "run `ollama serve`, or pass -skip-doctor and provide your own daemon"
		results = append(results, daemonResult)
		return results
	}
	results = append(results, daemonResult)

	// 3. models
	installed, err := listOllamaModels(ctx, endpoint)
	if err != nil {
		results = append(results, Result{
			Name:        "ollama-models",
			Severity:    FAIL,
			Message:     "failed to list installed models: " + err.Error(),
			Remediation: "investigate with `ollama list`",
		})
		return results
	}

	missing := make([]string, 0)
	for _, want := range required {
		if !containsCaseInsensitive(installed, want) {
			missing = append(missing, want)
		}
	}

	if len(missing) == 0 {
		results = append(results, Result{
			Name:     "ollama-models",
			Severity: OK,
			Message:  fmt.Sprintf("all %d required model(s) present", len(required)),
			Details:  []string{"required: " + strings.Join(required, ", ")},
		})
		return results
	}

	// Missing models. If interactive, prompt. Otherwise FAIL with guidance.
	if !opts.Interactive {
		results = append(results, Result{
			Name:        "ollama-models",
			Severity:    FAIL,
			Message:     fmt.Sprintf("%d required model(s) missing", len(missing)),
			Remediation: "run `aidev doctor` in an interactive terminal to fix, or `ollama pull <model>` manually",
			Details:     []string{"missing: " + strings.Join(missing, ", ")},
		})
		return results
	}

	// Interactive fix — one model at a time.
	fixResult := Result{Name: "ollama-models"}
	for _, want := range missing {
		choice, alt, err := promptMissingModel(opts, want, installed)
		if err != nil {
			fixResult.Severity = FAIL
			fixResult.Message = "prompt failed: " + err.Error()
			results = append(results, fixResult)
			return results
		}
		switch choice {
		case pullIt:
			fmt.Fprintf(opts.Out, "pulling %s (this may take a while)...\n", want)
			if err := pullOllamaModel(ctx, want); err != nil {
				fixResult.Severity = FAIL
				fixResult.Message = "pull failed: " + err.Error()
				fixResult.Remediation = "run `ollama pull " + want + "` manually and re-run aidev"
				results = append(results, fixResult)
				return results
			}
		case swapToInstalled:
			// Rewrite the in-memory routing to use the chosen alternative.
			// This is NOT persisted to models.yaml; the user can make the
			// change permanent themselves after they decide the alternative
			// works well enough.
			swapTierModel(cfg, want, alt)
			fmt.Fprintf(opts.Out, "swapped routing: %s -> %s (in memory only; edit models.yaml to persist)\n", want, alt)
		case abort:
			fixResult.Severity = FAIL
			fixResult.Message = "user declined to resolve missing model " + want
			fixResult.Remediation = "pull it (`ollama pull " + want + "`), or edit models.yaml to point at a model you do have"
			results = append(results, fixResult)
			return results
		}
	}
	fixResult.Severity = OK
	fixResult.Message = "all required models available after interactive fix"
	results = append(results, fixResult)
	return results
}

// requiredOllamaModels returns the unique set of model names referenced by
// ollama-typed tiers in the current routing. Non-ollama tiers are ignored.
func requiredOllamaModels(cfg *config.Config) []string {
	seen := map[string]bool{}
	var out []string
	for _, tier := range usedTiers(cfg) {
		if tier.Provider != "ollama" {
			continue
		}
		if seen[tier.Model] {
			continue
		}
		seen[tier.Model] = true
		out = append(out, tier.Model)
	}
	sort.Strings(out)
	return out
}

// usedTiers returns the subset of tiers that are actually referenced by
// the current routing. Defined tiers that aren't routed anywhere are
// ignored, to avoid spurious warnings about unused provider credentials.
func usedTiers(cfg *config.Config) []config.Tier {
	seen := map[string]bool{}
	var out []config.Tier
	for _, tierName := range cfg.Models.Routing {
		if seen[tierName] {
			continue
		}
		seen[tierName] = true
		if t, ok := cfg.Models.Tiers[tierName]; ok {
			out = append(out, t)
		}
	}
	return out
}

// firstOllamaEndpoint returns the endpoint of the first ollama tier in the
// routing. aidev currently assumes all ollama tiers share an endpoint;
// mixed-endpoint setups are a future problem.
func firstOllamaEndpoint(cfg *config.Config) string {
	for _, tierName := range cfg.Models.Routing {
		t, ok := cfg.Models.Tiers[tierName]
		if !ok || t.Provider != "ollama" {
			continue
		}
		if t.Endpoint != "" {
			return t.Endpoint
		}
	}
	if v := os.Getenv("AIDEV_OLLAMA_URL"); v != "" {
		return v
	}
	return "http://localhost:11434"
}

// pingOllama makes a cheap HEAD-ish call to /api/tags.
func pingOllama(ctx context.Context, endpoint string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/api/tags", nil)
	if err != nil {
		return false
	}
	c := &http.Client{Timeout: 2 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode < 400
}

// waitForOllama polls /api/tags every 500ms until it returns OK or the
// deadline expires.
func waitForOllama(ctx context.Context, endpoint string, deadline time.Duration) bool {
	stop := time.Now().Add(deadline)
	for time.Now().Before(stop) {
		if pingOllama(ctx, endpoint) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(500 * time.Millisecond):
		}
	}
	return false
}

// spawnOllama starts `ollama serve` in the background. The child is fully
// detached so it outlives aidev — the user asked for aidev not to own the
// daemon lifecycle. Stdio is nil so we don't leak `ollama serve` logs into
// the TTY once aidev takes over.
func spawnOllama() error {
	cmd := exec.Command("ollama", "serve")
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Start()
}

// listOllamaModels queries /api/tags and returns the list of model names
// currently pulled on the daemon.
func listOllamaModels(ctx context.Context, endpoint string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/api/tags", nil)
	if err != nil {
		return nil, err
	}
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var payload struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(payload.Models))
	for _, m := range payload.Models {
		out = append(out, m.Name)
	}
	return out, nil
}

// pullOllamaModel shells out to `ollama pull <name>`. We use the CLI rather
// than the /api/pull endpoint because the CLI gives the user a live
// progress bar in their own terminal, which is exactly what you want during
// a multi-GB download.
func pullOllamaModel(ctx context.Context, name string) error {
	cmd := exec.CommandContext(ctx, "ollama", "pull", name)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// containsCaseInsensitive reports whether needle appears in haystack. Ollama
// sometimes reports model names with a ":latest" tag suffix that the user's
// models.yaml may omit, so we also accept a "name:latest" match for a
// "name" query and vice versa.
func containsCaseInsensitive(haystack []string, needle string) bool {
	needleLC := strings.ToLower(needle)
	needleBase := strings.Split(needleLC, ":")[0]
	for _, h := range haystack {
		hlc := strings.ToLower(h)
		if hlc == needleLC {
			return true
		}
		hbase := strings.Split(hlc, ":")[0]
		needleHasTag := strings.Contains(needleLC, ":")
		hHasTag := strings.Contains(hlc, ":")
		// Accept "qwen2.5-coder:7b" vs "qwen2.5-coder:latest" as a match
		// only on the base name when exactly one side is ":latest".
		if hbase == needleBase {
			if hlc == hbase+":latest" && !needleHasTag {
				return true
			}
			if needleLC == needleBase+":latest" && !hHasTag {
				return true
			}
		}
	}
	return false
}

// swapTierModel rewrites every ollama tier that currently references old
// to reference newModel. Used by the interactive fix flow when the user
// picks "use an alternative installed model".
func swapTierModel(cfg *config.Config, old, newModel string) {
	for name, t := range cfg.Models.Tiers {
		if t.Provider == "ollama" && t.Model == old {
			t.Model = newModel
			cfg.Models.Tiers[name] = t
		}
	}
}
