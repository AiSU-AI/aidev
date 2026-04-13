package doctor

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/aisu-ai/aidev/internal/config"
)

func TestResultFormatOK(t *testing.T) {
	r := Result{Name: "config", Severity: OK, Message: "loaded"}
	got := r.Format()
	if !strings.Contains(got, "[OK]") || !strings.Contains(got, "config") || !strings.Contains(got, "loaded") {
		t.Errorf("missing expected content: %q", got)
	}
	if strings.Contains(got, "fix:") {
		t.Errorf("OK results should not print a fix line: %q", got)
	}
}

func TestResultFormatFailIncludesRemediation(t *testing.T) {
	r := Result{
		Name:        "ollama-daemon",
		Severity:    FAIL,
		Message:     "not reachable",
		Remediation: "run `ollama serve`",
	}
	got := r.Format()
	if !strings.Contains(got, "[FAIL]") {
		t.Errorf("missing FAIL label: %q", got)
	}
	if !strings.Contains(got, "fix: run `ollama serve`") {
		t.Errorf("missing remediation: %q", got)
	}
}

func TestResultFormatDetails(t *testing.T) {
	r := Result{
		Name:     "config",
		Severity: OK,
		Message:  "loaded",
		Details:  []string{"3 tiers defined", "6 roles routed"},
	}
	got := r.Format()
	if !strings.Contains(got, "3 tiers defined") || !strings.Contains(got, "6 roles routed") {
		t.Errorf("missing details: %q", got)
	}
}

func TestReportAnyAndHasFailure(t *testing.T) {
	rep := Report{Results: []Result{
		{Severity: OK},
		{Severity: WARN},
		{Severity: FAIL},
	}}
	if !rep.HasFailure() {
		t.Error("HasFailure false")
	}
	if !rep.Any(WARN) || !rep.Any(OK) {
		t.Error("Any(WARN|OK) false")
	}
	norep := Report{Results: []Result{{Severity: OK}}}
	if norep.HasFailure() {
		t.Error("HasFailure true on OK-only report")
	}
}

func TestReportWriteSummary(t *testing.T) {
	rep := Report{Results: []Result{
		{Name: "a", Severity: OK, Message: "ok"},
		{Name: "b", Severity: WARN, Message: "warn"},
		{Name: "c", Severity: FAIL, Message: "fail", Remediation: "fix it"},
	}}
	var buf bytes.Buffer
	rep.Write(&buf)
	out := buf.String()
	if !strings.Contains(out, "1 ok, 1 warn, 1 fail") {
		t.Errorf("summary line missing or wrong: %q", out)
	}
}

// TestCheckConfigFlagsMissingRouting verifies the config check rejects
// configs with no routing. Exercises the branch without needing real
// YAML files on disk.
func TestCheckConfigFlagsMissingRouting(t *testing.T) {
	cfg := &config.Config{
		Models: config.Models{
			Tiers: map[string]config.Tier{
				"small": {Provider: "ollama", Model: "x"},
			},
			// empty routing
		},
	}
	r := checkConfig(cfg)
	if r.Severity != FAIL {
		t.Errorf("severity = %v, want FAIL", r.Severity)
	}
}

func TestCheckConfigWarnsOnEmptyPrinciples(t *testing.T) {
	cfg := &config.Config{
		ConfigDir: "/tmp/x",
		Models: config.Models{
			Tiers:   map[string]config.Tier{"small": {Provider: "ollama", Model: "x"}},
			Routing: map[string]string{"scout": "small"},
		},
	}
	r := checkConfig(cfg)
	if r.Severity != WARN {
		t.Errorf("severity = %v, want WARN", r.Severity)
	}
}

// TestCheckAnthropicKeyFailsWhenRoutedAndMissing covers the critical
// branch: anthropic-typed tier in routing but no env var.
func TestCheckAnthropicKeyFailsWhenRoutedAndMissing(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	cfg := &config.Config{
		Models: config.Models{
			Tiers: map[string]config.Tier{
				"large": {Provider: "anthropic", Model: "claude-opus-4-6"},
			},
			Routing: map[string]string{"critic": "large"},
		},
	}
	r := checkAnthropicKey(cfg)
	if r.Severity != FAIL {
		t.Errorf("severity = %v, want FAIL", r.Severity)
	}
}

func TestCheckAnthropicKeyWarnsWhenNotRouted(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	cfg := &config.Config{
		Models: config.Models{
			Tiers:   map[string]config.Tier{"small": {Provider: "ollama", Model: "x"}},
			Routing: map[string]string{"scout": "small"},
		},
	}
	r := checkAnthropicKey(cfg)
	if r.Severity != WARN {
		t.Errorf("severity = %v, want WARN", r.Severity)
	}
}

func TestContainsCaseInsensitiveHandlesLatestSuffix(t *testing.T) {
	installed := []string{"qwen2.5-coder:latest", "llama3:8b"}
	if !containsCaseInsensitive(installed, "qwen2.5-coder") {
		t.Error("should match :latest when query omits tag")
	}
	installed2 := []string{"qwen2.5-coder"}
	if !containsCaseInsensitive(installed2, "qwen2.5-coder:latest") {
		t.Error("should match bare name when query says :latest")
	}
	installed3 := []string{"llama3:8b"}
	if containsCaseInsensitive(installed3, "qwen2.5-coder:7b") {
		t.Error("should not match unrelated names")
	}
	if !containsCaseInsensitive(installed3, "LLAMA3:8b") {
		t.Error("should be case-insensitive")
	}
}

func TestRequiredOllamaModelsIgnoresNonOllama(t *testing.T) {
	cfg := &config.Config{
		Models: config.Models{
			Tiers: map[string]config.Tier{
				"small":  {Provider: "ollama", Model: "qwen2.5-coder:7b"},
				"large":  {Provider: "anthropic", Model: "claude-opus-4-6"},
				"unused": {Provider: "ollama", Model: "should-not-appear"},
			},
			Routing: map[string]string{
				"scout":  "small",
				"critic": "large",
			},
		},
	}
	got := requiredOllamaModels(cfg)
	if len(got) != 1 || got[0] != "qwen2.5-coder:7b" {
		t.Errorf("got %v, want [qwen2.5-coder:7b]", got)
	}
}

func TestSwapTierModelReplacesAllOllamaReferencesToOld(t *testing.T) {
	cfg := &config.Config{
		Models: config.Models{
			Tiers: map[string]config.Tier{
				"small":  {Provider: "ollama", Model: "want"},
				"medium": {Provider: "ollama", Model: "other"},
				"large":  {Provider: "anthropic", Model: "want"}, // should NOT be touched
			},
		},
	}
	swapTierModel(cfg, "want", "replacement")
	if cfg.Models.Tiers["small"].Model != "replacement" {
		t.Errorf("small not swapped: %v", cfg.Models.Tiers["small"])
	}
	if cfg.Models.Tiers["medium"].Model != "other" {
		t.Errorf("medium should be untouched: %v", cfg.Models.Tiers["medium"])
	}
	if cfg.Models.Tiers["large"].Model != "want" {
		t.Errorf("anthropic tier should not be swapped: %v", cfg.Models.Tiers["large"])
	}
}

func TestPromptMissingModelPullChoice(t *testing.T) {
	opts := Options{
		Prompter: func(prompt string) (string, error) { return "1", nil },
	}
	choice, alt, err := promptMissingModel(opts, "qwen2.5-coder:7b", []string{"llama3:8b"})
	if err != nil {
		t.Fatal(err)
	}
	if choice != pullIt {
		t.Errorf("choice = %v, want pullIt", choice)
	}
	if alt != "" {
		t.Errorf("alt should be empty for pullIt, got %q", alt)
	}
}

func TestPromptMissingModelAbortsOnEmpty(t *testing.T) {
	opts := Options{
		Prompter: func(prompt string) (string, error) { return "", nil },
	}
	choice, _, err := promptMissingModel(opts, "qwen2.5-coder:7b", nil)
	if err != nil {
		t.Fatal(err)
	}
	if choice != abort {
		t.Errorf("choice = %v, want abort", choice)
	}
}

func TestPromptMissingModelSwapsWhenOptionTwoAndPickOne(t *testing.T) {
	calls := 0
	opts := Options{
		Prompter: func(prompt string) (string, error) {
			calls++
			if calls == 1 {
				return "2", nil
			}
			return "1", nil
		},
	}
	choice, alt, err := promptMissingModel(opts, "qwen2.5-coder:7b", []string{"llama3:8b", "deepseek-coder:6.7b"})
	if err != nil {
		t.Fatal(err)
	}
	if choice != swapToInstalled {
		t.Errorf("choice = %v, want swapToInstalled", choice)
	}
	if alt != "llama3:8b" {
		t.Errorf("alt = %q, want llama3:8b", alt)
	}
}

func TestPromptMissingModelRejectsOption2WithNoAlternatives(t *testing.T) {
	opts := Options{
		Prompter: func(prompt string) (string, error) { return "2", nil },
	}
	_, _, err := promptMissingModel(opts, "qwen2.5-coder:7b", nil)
	if err == nil {
		t.Error("expected error when picking option 2 with no alternatives")
	}
}

// TestRunSkipsOllamaWhenNoOllamaRouting ensures the full Run entry point
// doesn't try to talk to Ollama when the routing has no ollama tiers.
func TestRunSkipsOllamaWhenNoOllamaRouting(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")
	cfg := &config.Config{
		ConfigDir: "/tmp/x",
		Models: config.Models{
			Tiers: map[string]config.Tier{
				"large": {Provider: "anthropic", Model: "claude-opus-4-6"},
			},
			Routing: map[string]string{
				"scout":  "large",
				"critic": "large",
			},
		},
		Principles: config.Principles{Principles: []config.Principle{{Name: "test"}}},
	}
	rep := Run(context.Background(), cfg, Options{})
	// Should have config + anthropic + one ollama "skipping" result.
	for _, r := range rep.Results {
		if r.Name == "ollama-binary" || r.Name == "ollama-daemon" || r.Name == "ollama-models" {
			t.Errorf("should not have emitted %q when no ollama routing", r.Name)
		}
	}
	if rep.HasFailure() {
		t.Errorf("should not fail when routing is all-anthropic with key set: %+v", rep.Results)
	}
}
