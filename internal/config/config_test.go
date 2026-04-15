package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const principlesYAML = `
principles:
  - name: Test Principle
    summary: Example
    description: For tests only.
`

// modelsYAMLWithTwoProfiles is the v0.5 shape: profiles map +
// active_profile pointer at the top level. Used by most tests
// below to verify the new loader produces the expected resolved
// Config.
const modelsYAMLWithTwoProfiles = `
active_profile: default

profiles:
  default:
    description: Local test profile.
    tiers:
      small:
        provider: ollama
        model: foo:7b
        endpoint: http://localhost:11434
        max_tokens: 2048
        temperature: 0.2
      cloud_large:
        provider: claude-cli
        model: ""
        max_tokens: 8192
        temperature: 0.3
      oversight:
        provider: claude-cli
        model: ""
        max_tokens: 8192
        temperature: 0.3
    routing:
      scout: small
      tester: small
      implementer: small
      reviewer: small
      critic: cloud_large
      architect: cloud_large
      charter: cloud_large
      coordinator: oversight

  offline:
    description: Fully offline.
    tiers:
      bare:
        provider: ollama
        model: bar:14b
        max_tokens: 4096
        temperature: 0.1
    routing:
      scout: bare
      critic: bare
      architect: bare
      implementer: bare
      reviewer: bare
      charter: bare
      tester: bare
      coordinator: bare
`

func writeFiles(t *testing.T, dir, models string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "models.yaml"), []byte(models), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "principles.yaml"), []byte(principlesYAML), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestLoadResolvesActiveProfile is the baseline: a v0.5 file with
// an explicit active_profile must produce a Config whose Models
// reflects that profile's tiers + routing, and Config.ActiveProfile
// must be set so the startup banner can show it.
func TestLoadResolvesActiveProfile(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, modelsYAMLWithTwoProfiles)

	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ActiveProfile != "default" {
		t.Errorf("ActiveProfile = %q, want default", cfg.ActiveProfile)
	}
	if cfg.Models.Routing["coordinator"] != "oversight" {
		t.Errorf("coordinator routing = %q, want oversight", cfg.Models.Routing["coordinator"])
	}
	if cfg.Models.Tiers["oversight"].Provider != "claude-cli" {
		t.Errorf("oversight tier provider = %q", cfg.Models.Tiers["oversight"].Provider)
	}
	if len(cfg.Principles.Principles) != 1 {
		t.Errorf("principles count = %d, want 1", len(cfg.Principles.Principles))
	}
	// RawModelsFile must be populated so config-edit tooling can
	// round-trip the file without losing the `offline` profile.
	if cfg.RawModelsFile == nil {
		t.Fatal("RawModelsFile not populated")
	}
	if _, ok := cfg.RawModelsFile.Profiles["offline"]; !ok {
		t.Errorf("RawModelsFile should retain non-active 'offline' profile for round-tripping")
	}
}

// TestLoadRejectsMissingActiveProfile verifies that a file with
// profiles but no active_profile field errors loudly. We never
// silently pick a default — the user has to declare which profile
// is active.
func TestLoadRejectsMissingActiveProfile(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, `
profiles:
  default:
    tiers:
      small: {provider: ollama, model: x}
    routing:
      scout: small
`)
	_, err := Load(dir)
	if err == nil {
		t.Fatal("expected error for missing active_profile")
	}
	if !strings.Contains(err.Error(), "active_profile") {
		t.Errorf("error should mention active_profile; got: %v", err)
	}
}

// TestLoadRejectsActiveProfileNamingNonexistent guards against a
// typo in active_profile pointing at a profile that doesn't
// exist. Error must list the available profiles so the user can
// fix it without opening the file.
func TestLoadRejectsActiveProfileNamingNonexistent(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, `
active_profile: typo
profiles:
  default:
    tiers:
      small: {provider: ollama, model: x}
    routing:
      scout: small
  offline:
    tiers:
      bare: {provider: ollama, model: y}
    routing:
      scout: bare
`)
	_, err := Load(dir)
	if err == nil {
		t.Fatal("expected error for nonexistent active_profile")
	}
	for _, want := range []string{"typo", "default", "offline"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q; got: %v", want, err)
		}
	}
}

// TestLoadRejectsDanglingRouting verifies that a profile's
// routing referencing an unknown tier is caught at load time.
// Same guarantee as the v0.4 loader had, just scoped to the
// active profile.
func TestLoadRejectsDanglingRouting(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, `
active_profile: default
profiles:
  default:
    tiers:
      small: {provider: ollama, model: x}
    routing:
      scout: small
      critic: nonexistent
`)
	_, err := Load(dir)
	if err == nil {
		t.Error("expected error for dangling routing")
	}
}

// TestLoadRejectsEmptyTierProvider verifies that a tier with no
// provider field is caught. We don't want a silent
// "construct provider from empty string" failure deeper in the
// router.
func TestLoadRejectsEmptyTierProvider(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, `
active_profile: default
profiles:
  default:
    tiers:
      small: {model: x}
    routing:
      scout: small
`)
	_, err := Load(dir)
	if err == nil {
		t.Error("expected error for tier with empty provider")
	}
}

// TestLoadRejectsEmptyProfilesMap is the negative case for an
// empty file shell.
func TestLoadRejectsEmptyProfilesMap(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, "active_profile: default\nprofiles: {}\n")
	_, err := Load(dir)
	if err == nil {
		t.Error("expected error for empty profiles map")
	}
}

// TestLoadDetectsLegacyV04Format is the migration-aid: a file
// with top-level tiers + routing (v0.4 shape) instead of profiles
// must error with a clear message pointing the user at
// `aidev install --force`. Without this, the user would get a
// confusing "active_profile field missing" error and have no
// idea what to do.
func TestLoadDetectsLegacyV04Format(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, `
tiers:
  small:
    provider: ollama
    model: foo:7b
    max_tokens: 2048
    temperature: 0.2
  large:
    provider: claude-cli
    model: ""
    max_tokens: 8192
    temperature: 0.3
routing:
  scout: small
  critic: large
`)
	_, err := Load(dir)
	if err == nil {
		t.Fatal("expected error for legacy v0.4 format")
	}
	if !strings.Contains(err.Error(), "legacy v0.4 format") {
		t.Errorf("error should mention legacy format; got: %v", err)
	}
	if !strings.Contains(err.Error(), "aidev install --force") {
		t.Errorf("error should suggest the fix; got: %v", err)
	}
}

// TestProfileNamesIsSorted is a tiny guard on the helper used for
// error messages — sorted output keeps "available profiles: [...]"
// stable across runs.
// TestLoadParsesTimeoutSeconds verifies that the v0.5b
// timeout_seconds field on a tier is deserialized into the
// Tier struct so Router.buildProvider can plumb it through to
// the provider constructors. Regression guard against the
// YAML tag drifting or the field being dropped in a refactor.
func TestLoadParsesTimeoutSeconds(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, `
active_profile: default
profiles:
  default:
    tiers:
      small:
        provider: ollama
        model: qwen:7b
        max_tokens: 2048
        temperature: 0.2
        timeout_seconds: 900
      large:
        provider: ollama
        model: qwen:32b
        max_tokens: 8192
        temperature: 0.2
        timeout_seconds: 1800
      no_timeout:
        provider: ollama
        model: qwen:14b
    routing:
      scout: small
      critic: large
      architect: large
      charter: large
      implementer: no_timeout
      reviewer: no_timeout
      tester: small
      coordinator: large
`)
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Models.Tiers["small"].TimeoutSeconds; got != 900 {
		t.Errorf("small.timeout_seconds = %d, want 900", got)
	}
	if got := cfg.Models.Tiers["large"].TimeoutSeconds; got != 1800 {
		t.Errorf("large.timeout_seconds = %d, want 1800", got)
	}
	// Zero when omitted — this means "use the provider's
	// default", not an error.
	if got := cfg.Models.Tiers["no_timeout"].TimeoutSeconds; got != 0 {
		t.Errorf("no_timeout.timeout_seconds = %d, want 0 (unset)", got)
	}
}

func TestProfileNamesIsSorted(t *testing.T) {
	in := map[string]Profile{
		"zzz":     {},
		"aaa":     {},
		"default": {},
	}
	got := profileNames(in)
	want := []string{"aaa", "default", "zzz"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got %v, want %v", got, want)
		}
	}
}
