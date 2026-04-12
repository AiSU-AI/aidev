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
	"errors"
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
}

// Load reads models.yaml and principles.yaml from dir. An empty dir defaults
// to "./config".
func Load(dir string) (*Config, error) {
	if dir == "" {
		dir = "config"
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve config dir: %w", err)
	}

	var cfg Config
	cfg.ConfigDir = abs

	if err := readYAML(filepath.Join(abs, "models.yaml"), &cfg.Models); err != nil {
		return nil, fmt.Errorf("models.yaml: %w", err)
	}
	if err := readYAML(filepath.Join(abs, "principles.yaml"), &cfg.Principles); err != nil {
		return nil, fmt.Errorf("principles.yaml: %w", err)
	}

	if len(cfg.Models.Tiers) == 0 {
		return nil, errors.New("models.yaml: no tiers defined")
	}
	if len(cfg.Models.Routing) == 0 {
		return nil, errors.New("models.yaml: no routing defined")
	}
	for role, tier := range cfg.Models.Routing {
		if _, ok := cfg.Models.Tiers[tier]; !ok {
			return nil, fmt.Errorf("models.yaml: role %q routed to unknown tier %q", role, tier)
		}
	}

	return &cfg, nil
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
