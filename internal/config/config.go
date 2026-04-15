// Package config loads aidev's runtime configuration from YAML files on disk.
//
// Two files are expected under the config directory:
//
//   - models.yaml:     named profiles + active profile pointer.
//                      Each profile bundles a set of model tiers and a
//                      role->tier routing. The active profile is what
//                      aidev resolves on startup.
//   - principles.yaml: the engineering principles the Critic evaluates
//                      against. Orthogonal to model routing — same
//                      across every profile.
//
// v0.5: the legacy "tiers + routing at the top level + --preset
// flag pointing at alternate models.<name>.yaml files" scheme is
// gone. Everything lives in one file with named profiles. See
// `aidev config` subcommands for the supported edit surface.
package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Tier describes a single model backend that aidev can route work to.
// Identical to the legacy shape — only the surrounding container
// changed in v0.5.
type Tier struct {
	Provider    string  `yaml:"provider"`
	Model       string  `yaml:"model"`
	Endpoint    string  `yaml:"endpoint,omitempty"`
	MaxTokens   int     `yaml:"max_tokens"`
	Temperature float64 `yaml:"temperature"`
}

// Profile is one named bundle of tiers + routing. A models.yaml
// file defines a set of profiles and marks one as active via the
// top-level ActiveProfile field.
//
// Description is shown by `aidev config profile list` so the user
// can pick between profiles without opening the file. Keep it to
// one or two sentences; the file's preamble is the right place for
// long-form documentation.
type Profile struct {
	Description string            `yaml:"description"`
	Tiers       map[string]Tier   `yaml:"tiers"`
	Routing     map[string]string `yaml:"routing"`
}

// ModelsFile is the deserialized on-disk shape of models.yaml. It
// stays alongside the resolved Models below so config-edit tooling
// (`aidev config set`, `aidev config profile use`) can mutate one
// field and write the whole file back without losing any other
// profiles or comments.
//
// Note: comments in the source YAML are NOT preserved across
// round-tripping with go-yaml v3's default Marshal. The tooling
// works around this by editing the user's existing file in place
// (preserving comments) when possible, and falling back to a
// regenerated file with the standard preamble when the file is
// being created from scratch.
type ModelsFile struct {
	ActiveProfile string             `yaml:"active_profile"`
	Profiles      map[string]Profile `yaml:"profiles"`
}

// Principle is a single rule the Critic uses when arguing about a
// proposal.
type Principle struct {
	Name        string `yaml:"name"`
	Summary     string `yaml:"summary"`
	Description string `yaml:"description"`
}

// Principles holds the full principles.yaml file.
type Principles struct {
	Principles []Principle `yaml:"principles"`
}

// Models is the RESOLVED routing for the currently active profile —
// the same shape downstream code (Router, orchestrator, agents) has
// always seen. v0.5 derives this from the active profile inside
// ModelsFile rather than reading it from a top-level YAML block.
//
// Kept as its own type instead of inlining into Config so the
// existing `cfg.Models.Tiers` / `cfg.Models.Routing` access pattern
// in downstream packages stays unchanged.
type Models struct {
	Tiers   map[string]Tier
	Routing map[string]string
}

// Config is the aggregate runtime configuration produced by Load.
type Config struct {
	// Models is the resolved active profile's tiers + routing,
	// flattened for convenient downstream access.
	Models Models

	// Principles is the parsed principles.yaml.
	Principles Principles

	// ConfigDir is the directory the files were loaded from. Used
	// for clear error messages and as the target for write-back
	// from `aidev config` subcommands.
	ConfigDir string

	// ActiveProfile is the name of the profile currently in use.
	// Surfaced by the startup banner and `aidev config show`.
	ActiveProfile string

	// RawModelsFile is the full deserialized models.yaml, preserved
	// so config-edit tooling can mutate one field and write the
	// whole file back without losing other profiles. Downstream
	// agent code should NEVER read this — use Models instead.
	RawModelsFile *ModelsFile
}

// Load reads models.yaml + principles.yaml from dir, resolves the
// active profile, and returns a Config ready to hand to the Router
// and orchestrator. An empty dir defaults to "config".
//
// Loud errors on any of:
//
//   - models.yaml missing or unreadable
//   - models.yaml in the legacy v0.4 shape (top-level tiers +
//     routing instead of profiles) — error tells the user to run
//     `aidev install --force` to migrate
//   - active_profile field missing or empty
//   - active_profile names a profile that doesn't exist
//   - any tier in the active profile has an empty provider
//   - any role in the active profile's routing references an
//     unknown tier
//
// We never silently fall back to a default profile or default
// routing. If the file is broken, the user sees a clear message
// and the process exits.
func Load(dir string) (*Config, error) {
	if dir == "" {
		dir = "config"
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve config dir: %w", err)
	}

	data, err := os.ReadFile(filepath.Join(abs, "models.yaml"))
	if err != nil {
		return nil, fmt.Errorf("models.yaml: %w", err)
	}

	// Detect the legacy v0.4 shape (top-level `tiers:` and
	// `routing:` blocks with no `profiles:` field) and bail with
	// a clear migration message instead of trying to parse it as
	// the new shape.
	if isLegacyModelsYAML(data) {
		return nil, fmt.Errorf("models.yaml: legacy v0.4 format detected (top-level tiers/routing). Run `aidev install --force` to install the new profile-based config, then re-apply any customizations via `aidev config set`. Your old file will be backed up to models.yaml.bak.")
	}

	var raw ModelsFile
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("models.yaml: parse: %w", err)
	}

	cfg := &Config{
		ConfigDir:     abs,
		RawModelsFile: &raw,
		ActiveProfile: raw.ActiveProfile,
	}

	if raw.ActiveProfile == "" {
		return nil, fmt.Errorf("models.yaml: missing top-level `active_profile` field")
	}
	if len(raw.Profiles) == 0 {
		return nil, fmt.Errorf("models.yaml: no profiles defined")
	}
	profile, ok := raw.Profiles[raw.ActiveProfile]
	if !ok {
		available := profileNames(raw.Profiles)
		return nil, fmt.Errorf("models.yaml: active_profile=%q does not exist; available profiles: %v", raw.ActiveProfile, available)
	}
	if err := validateProfile(raw.ActiveProfile, profile); err != nil {
		return nil, err
	}

	cfg.Models = Models{
		Tiers:   profile.Tiers,
		Routing: profile.Routing,
	}

	if err := readYAML(filepath.Join(abs, "principles.yaml"), &cfg.Principles); err != nil {
		return nil, fmt.Errorf("principles.yaml: %w", err)
	}

	return cfg, nil
}

// validateProfile enforces the same invariants the legacy loader
// did: tiers must be non-empty with a provider set, routing must
// reference real tiers. Returns errors with the profile name
// embedded so multi-profile validation is easy to read.
func validateProfile(name string, p Profile) error {
	if len(p.Tiers) == 0 {
		return fmt.Errorf("profile %q: no tiers defined", name)
	}
	if len(p.Routing) == 0 {
		return fmt.Errorf("profile %q: no routing defined", name)
	}
	for tierName, tier := range p.Tiers {
		if tier.Provider == "" {
			return fmt.Errorf("profile %q: tier %q has empty provider", name, tierName)
		}
	}
	for role, tierName := range p.Routing {
		if _, ok := p.Tiers[tierName]; !ok {
			return fmt.Errorf("profile %q: role %q routed to unknown tier %q", name, role, tierName)
		}
	}
	return nil
}

// isLegacyModelsYAML uses a shallow check to detect a v0.4-shape
// file: top-level `tiers:` AND `routing:` keys with no
// `profiles:` key. We don't fully parse the file because the goal
// is to give a clear migration message before the new-format
// parser produces a confusing structural error.
func isLegacyModelsYAML(data []byte) bool {
	var probe struct {
		Tiers    map[string]Tier    `yaml:"tiers"`
		Routing  map[string]string  `yaml:"routing"`
		Profiles map[string]Profile `yaml:"profiles"`
	}
	if err := yaml.Unmarshal(data, &probe); err != nil {
		return false
	}
	return len(probe.Profiles) == 0 && (len(probe.Tiers) > 0 || len(probe.Routing) > 0)
}

// profileNames returns a sorted slice of profile keys for
// consistent error messages. Sorted because Go map iteration is
// random and `available profiles: [a b]` is much friendlier to
// debug than `[b a]` on one run and the reverse on the next.
func profileNames(profiles map[string]Profile) []string {
	names := make([]string, 0, len(profiles))
	for name := range profiles {
		names = append(names, name)
	}
	// Inline insertion sort to avoid a sort import for a tiny slice.
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j-1] > names[j]; j-- {
			names[j-1], names[j] = names[j], names[j-1]
		}
	}
	return names
}

// MergePrinciples appends additional principles (e.g. loaded from
// the target repo) to the global set. Duplicates by name are skipped
// so the global config wins.
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
