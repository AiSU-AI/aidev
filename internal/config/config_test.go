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
