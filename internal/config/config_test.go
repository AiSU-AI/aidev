package config

import (
	"os"
	"path/filepath"
	"testing"
)

const modelsYAML = `
tiers:
  small:
    provider: ollama
    model: foo:7b
    endpoint: http://localhost:11434
    max_tokens: 2048
    temperature: 0.2
  large:
    provider: anthropic
    model: claude-opus-4-6
    max_tokens: 8192
    temperature: 0.3
routing:
  scout: small
  critic: large
`

const principlesYAML = `
principles:
  - name: Test Principle
    summary: Example
    description: For tests only.
`

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "models.yaml"), []byte(modelsYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "principles.yaml"), []byte(principlesYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Models.Tiers) != 2 {
		t.Errorf("tiers = %d, want 2", len(cfg.Models.Tiers))
	}
	if cfg.Models.Routing["scout"] != "small" {
		t.Errorf("scout routing = %q, want small", cfg.Models.Routing["scout"])
	}
	if len(cfg.Principles.Principles) != 1 {
		t.Errorf("principles = %d, want 1", len(cfg.Principles.Principles))
	}
}

func TestLoadRejectsDanglingRouting(t *testing.T) {
	dir := t.TempDir()
	bad := `
tiers:
  small:
    provider: ollama
    model: foo
routing:
  scout: nonexistent
`
	_ = os.WriteFile(filepath.Join(dir, "models.yaml"), []byte(bad), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "principles.yaml"), []byte(principlesYAML), 0o644)
	if _, err := Load(dir); err == nil {
		t.Error("expected error for routing to unknown tier")
	}
}

// TestLoadPresetReadsAlternateModelsFile verifies that --preset local
// swaps the models file from models.yaml to models.local.yaml without
// touching principles.yaml. This is the core guarantee of the preset
// feature: alternate routing bundles without duplicating principles.
func TestLoadPresetReadsAlternateModelsFile(t *testing.T) {
	dir := t.TempDir()
	// Default file uses a cloud tier; preset file is fully local.
	_ = os.WriteFile(filepath.Join(dir, "models.yaml"), []byte(modelsYAML), 0o644)
	localYAML := `
tiers:
  small:
    provider: ollama
    model: qwen2.5-coder:7b
    endpoint: http://localhost:11434
    max_tokens: 2048
    temperature: 0.2
routing:
  scout: small
  critic: small
`
	_ = os.WriteFile(filepath.Join(dir, "models.local.yaml"), []byte(localYAML), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "principles.yaml"), []byte(principlesYAML), 0o644)

	cfg, err := LoadPreset(dir, "local")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Preset != "local" {
		t.Errorf("cfg.Preset = %q, want local", cfg.Preset)
	}
	// The preset routes both scout and critic to small — proof we
	// read models.local.yaml and not the default.
	if cfg.Models.Routing["critic"] != "small" {
		t.Errorf("critic routing = %q, want small (local preset)", cfg.Models.Routing["critic"])
	}
	if cfg.Models.Tiers["small"].Model != "qwen2.5-coder:7b" {
		t.Errorf("small tier model = %q, want qwen2.5-coder:7b", cfg.Models.Tiers["small"].Model)
	}
	// Principles are shared across presets; should still load.
	if len(cfg.Principles.Principles) != 1 {
		t.Errorf("principles = %d, want 1", len(cfg.Principles.Principles))
	}
}

// TestLoadPresetMissingFileIsLoud is the negative case: asking for a
// preset that doesn't exist must error, not silently fall back to the
// default. Silent fallback would hide the user's intent and ship the
// wrong tier routing without warning.
func TestLoadPresetMissingFileIsLoud(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "models.yaml"), []byte(modelsYAML), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "principles.yaml"), []byte(principlesYAML), 0o644)

	_, err := LoadPreset(dir, "nonexistent")
	if err == nil {
		t.Fatal("expected error when preset file is missing")
	}
}

// TestLoadPresetRejectsUnsafeNames guards against path-traversal via
// the preset name. Anything that isn't [A-Za-z0-9_-]+ must be refused
// before it becomes part of a filename.
func TestLoadPresetRejectsUnsafeNames(t *testing.T) {
	cases := []string{"..", "../etc", "foo/bar", "has space", "has.dot", ""}
	for _, c := range cases {
		_, err := LoadPreset(t.TempDir(), c)
		if err == nil {
			t.Errorf("expected error for unsafe preset name %q", c)
		}
	}
}

// TestLoadWithoutPresetIsUnchanged verifies backwards compatibility:
// callers that never pass a preset still get the default models.yaml
// and Config.Preset is empty.
func TestLoadWithoutPresetIsUnchanged(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "models.yaml"), []byte(modelsYAML), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "principles.yaml"), []byte(principlesYAML), 0o644)

	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Preset != "" {
		t.Errorf("cfg.Preset = %q, want empty for default load", cfg.Preset)
	}
	if cfg.Models.Routing["critic"] != "large" {
		t.Errorf("critic routing = %q, want large (default)", cfg.Models.Routing["critic"])
	}
}
