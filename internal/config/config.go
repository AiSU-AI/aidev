// Package config loads aidev's runtime configuration from YAML files on disk.
//
// Two config files are expected:
//
//   - models.yaml:     model tier definitions + role->tier routing
//   - principles.yaml: the engineering principles the Critic evaluates against
//
// Both files live under ./config relative to the binary by default, but the
// loader accepts an explicit path so tests and alternate layouts work.
package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Tier describes a single model backend that aidev can route work to.
type Tier struct {
	Provider    string  `yaml:"provider"`
	Model       string  `yaml:"model"`
	Endpoint    string  `yaml:"endpoint,omitempty"`
	MaxTokens   int     `yaml:"max_tokens"`
	Temperature float64 `yaml:"temperature"`
}

// Models holds the full models.yaml file.
type Models struct {
	Tiers   map[string]Tier   `yaml:"tiers"`
	Routing map[string]string `yaml:"routing"`
}

// Principle is a single rule the Critic uses when arguing about a proposal.
type Principle struct {
	Name        string `yaml:"name"`
	Summary     string `yaml:"summary"`
	Description string `yaml:"description"`
}

// Principles holds the full principles.yaml file.
type Principles struct {
	Principles []Principle `yaml:"principles"`
}

// Config is the aggregate runtime configuration.
type Config struct {
	Models     Models
	Principles Principles
	// ConfigDir is the directory the files were loaded from, used for
	// producing clear error messages.
	ConfigDir string
	// Preset is the non-empty preset name (e.g. "local") that was used
	// to load Models, or "" if the default models.yaml was used.
	// Surfaced so the doctor and the headless report can tell the user
	// which routing is active.
	Preset string
}

// Load reads models.yaml and principles.yaml from dir. An empty dir defaults
// to "./config". Equivalent to LoadPreset(dir, "").
func Load(dir string) (*Config, error) {
	return LoadPreset(dir, "")
}

// LoadPreset is the preset-aware form of Load. When preset is empty, it
// reads the default `models.yaml` (same as Load). When preset is non-empty,
// it reads `models.<preset>.yaml` instead — allowing shipped bundles like
// `local` (all-local Ollama routing) or user-authored alternatives
// (`models.offline.yaml`, `models.gpu-rich.yaml`, ...) to swap routing
// without touching the default file.
//
// Principles always load from `principles.yaml`; presets only affect the
// model routing, not the engineering principles the Critic evaluates
// against. If a user wants preset-specific principles they can still
// override via .aidev/principles.yaml in the target repo.
//
// A missing preset file is a loud error — we never silently fall back to
// the default. If you asked for `--preset local` and there is no
// models.local.yaml in the config directory, you want to know.
func LoadPreset(dir, preset string) (*Config, error) {
	if dir == "" {
		dir = "config"
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve config dir: %w", err)
	}

	modelsFile := "models.yaml"
	if preset != "" {
		if !isSafePresetName(preset) {
			return nil, fmt.Errorf("invalid preset name %q (letters, digits, dash, underscore only)", preset)
		}
		modelsFile = "models." + preset + ".yaml"
	}

	var cfg Config
	cfg.ConfigDir = abs
	cfg.Preset = preset

	if err := readYAML(filepath.Join(abs, modelsFile), &cfg.Models); err != nil {
		if preset != "" {
			return nil, fmt.Errorf("%s: %w (preset %q not found in %s)", modelsFile, err, preset, abs)
		}
		return nil, fmt.Errorf("%s: %w", modelsFile, err)
	}
	if err := readYAML(filepath.Join(abs, "principles.yaml"), &cfg.Principles); err != nil {
		return nil, fmt.Errorf("principles.yaml: %w", err)
	}

	if len(cfg.Models.Tiers) == 0 {
		return nil, fmt.Errorf("%s: no tiers defined", modelsFile)
	}
	if len(cfg.Models.Routing) == 0 {
		return nil, fmt.Errorf("%s: no routing defined", modelsFile)
	}
	for role, tier := range cfg.Models.Routing {
		if _, ok := cfg.Models.Tiers[tier]; !ok {
			return nil, fmt.Errorf("%s: role %q routed to unknown tier %q", modelsFile, role, tier)
		}
	}

	return &cfg, nil
}

// isSafePresetName returns true for preset names that are safe to
// substitute into a filename. We enforce a conservative character set
// rather than shell-quoting because preset names should be short,
// human-readable identifiers like `local`, `offline`, `gpu-rich` — not
// arbitrary user input.
func isSafePresetName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_':
		default:
			return false
		}
	}
	return true
}

// MergePrinciples appends additional principles (e.g. loaded from the target
// repo) to the global set. Duplicates by name are skipped so the global
// config wins.
func (c *Config) MergePrinciples(extra []Principle) {
	have := make(map[string]bool, len(c.Principles.Principles))
	for _, p := range c.Principles.Principles {
		have[p.Name] = true
	}
	for _, p := range extra {
		if have[p.Name] {
			continue
		}
		c.Principles.Principles = append(c.Principles.Principles, p)
		have[p.Name] = true
	}
}

func readYAML(path string, out any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return yaml.Unmarshal(data, out)
}
