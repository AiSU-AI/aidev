package main

// Tests for the `aidev init` subcommand. We exercise the pure
// helpers and the file-writing path directly; the interactive
// flow (promptForProfile, offerOllamaInstall) is deliberately
// untested here because it shells out to package managers and
// reads stdin — not worth the ceremony of mocking. Those paths
// are covered by manual verification from the acceptance notes.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestIsValidInitProfileAcceptsShippedNames guards the menu
// descriptor list against drift from the shipped config. If a user
// adds a profile to config/models.yaml without adding a descriptor
// here, `aidev init --profile <new>` would reject it. This test
// doesn't catch that (it tests the descriptor list itself), but it
// does catch accidental deletion of a descriptor — a bug that would
// silently strand users on an old menu.
func TestIsValidInitProfileAcceptsShippedNames(t *testing.T) {
	want := []string{"cloud-only", "default", "high-vram", "low-vram", "offline"}
	for _, name := range want {
		if !isValidInitProfile(name) {
			t.Errorf("isValidInitProfile(%q) = false, want true — did you remove a descriptor?", name)
		}
	}
	if isValidInitProfile("nonsense") {
		t.Errorf("isValidInitProfile(\"nonsense\") = true, want false")
	}
	if isValidInitProfile("") {
		t.Errorf("isValidInitProfile(\"\") = true, want false")
	}
}

// TestInitProfileByNameReturnsOllamaFlag confirms the descriptor
// table correctly reports which profiles need Ollama. The init flow
// uses this flag to decide whether to offer an install or skip
// straight to writing the config — a miscoded flag here could
// either (a) spam the user with a pointless install prompt for
// cloud-only, or (b) silently write a broken config for default
// without warning about missing Ollama.
func TestInitProfileByNameReturnsOllamaFlag(t *testing.T) {
	cases := []struct {
		name           string
		wantRequires   bool
	}{
		{"cloud-only", false},
		{"default", true},
		{"high-vram", true},
		{"low-vram", true},
		{"offline", true},
	}
	for _, tc := range cases {
		got := initProfileByName(tc.name)
		if got.name != tc.name {
			t.Errorf("initProfileByName(%q).name = %q, want %q", tc.name, got.name, tc.name)
		}
		if got.requiresOllama != tc.wantRequires {
			t.Errorf("initProfileByName(%q).requiresOllama = %v, want %v", tc.name, got.requiresOllama, tc.wantRequires)
		}
	}
	zero := initProfileByName("nonexistent")
	if zero.name != "" {
		t.Errorf("initProfileByName(\"nonexistent\").name = %q, want empty", zero.name)
	}
}

// TestWriteInitConfigCloudOnly is the happy-path integration test:
// calling writeInitConfig(dir, "cloud-only", true) against an empty
// dir produces a valid models.yaml with active_profile=cloud-only.
// This is what `aidev init --profile cloud-only --yes` does under
// the hood.
func TestWriteInitConfigCloudOnly(t *testing.T) {
	dir := t.TempDir()

	if err := writeInitConfig(dir, "cloud-only", false); err != nil {
		t.Fatalf("writeInitConfig: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "models.yaml"))
	if err != nil {
		t.Fatalf("read models.yaml: %v", err)
	}
	var file struct {
		ActiveProfile string `yaml:"active_profile"`
	}
	if err := yaml.Unmarshal(raw, &file); err != nil {
		t.Fatalf("unmarshal models.yaml: %v", err)
	}
	if file.ActiveProfile != "cloud-only" {
		t.Errorf("active_profile = %q, want %q", file.ActiveProfile, "cloud-only")
	}

	// Every shipped profile should survive — writeInitConfig must
	// not drop anything when switching the active profile.
	var full struct {
		Profiles map[string]struct{} `yaml:"profiles"`
	}
	if err := yaml.Unmarshal(raw, &full); err != nil {
		t.Fatalf("reparse for profile list: %v", err)
	}
	wantProfiles := []string{"cloud-only", "default", "high-vram", "low-vram", "offline"}
	for _, name := range wantProfiles {
		if _, ok := full.Profiles[name]; !ok {
			t.Errorf("profile %q missing after writeInitConfig — did switching active_profile drop a profile?", name)
		}
	}
}

// TestWriteInitConfigDefaultDoesNotRewrite guards the fast path
// when the user's pick already matches the shipped default
// (active_profile=default in the shipped file): writeInitConfig
// should not call writeActiveProfile. We can't observe that
// directly, but we can confirm the file content still parses and
// still names default.
func TestWriteInitConfigDefaultDoesNotRewrite(t *testing.T) {
	dir := t.TempDir()
	if err := writeInitConfig(dir, "default", false); err != nil {
		t.Fatalf("writeInitConfig: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "models.yaml"))
	if err != nil {
		t.Fatalf("read models.yaml: %v", err)
	}
	if !strings.Contains(string(raw), "active_profile: default") {
		t.Errorf("expected active_profile: default in shipped file; got:\n%s", firstNLines(string(raw), 10))
	}
}

// TestWriteInitConfigRejectsExistingWithoutForce is the guard that
// protects a user's hand-edited models.yaml from being clobbered
// by a second `aidev init` run. A scripted caller that really
// wants to overwrite must pass --force.
func TestWriteInitConfigRejectsExistingWithoutForce(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "models.yaml")
	if err := os.WriteFile(existing, []byte("# user-edited\nactive_profile: default\nprofiles: {}\n"), 0o644); err != nil {
		t.Fatalf("seed existing file: %v", err)
	}

	err := writeInitConfig(dir, "cloud-only", false)
	if err == nil {
		t.Fatal("writeInitConfig should refuse to overwrite without --force, got nil error")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error should mention 'already exists', got: %v", err)
	}
	// File must be untouched.
	got, readErr := os.ReadFile(existing)
	if readErr != nil {
		t.Fatalf("read seeded file: %v", readErr)
	}
	if !strings.Contains(string(got), "# user-edited") {
		t.Errorf("existing file was modified despite refusal; got:\n%s", got)
	}
}

// TestWriteInitConfigOverwritesWithForce confirms the --force
// escape hatch actually works — otherwise users with a broken
// existing config could never re-init.
func TestWriteInitConfigOverwritesWithForce(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "models.yaml")
	if err := os.WriteFile(existing, []byte("# stale\nactive_profile: default\nprofiles: {}\n"), 0o644); err != nil {
		t.Fatalf("seed existing file: %v", err)
	}

	if err := writeInitConfig(dir, "cloud-only", true); err != nil {
		t.Fatalf("writeInitConfig with force=true: %v", err)
	}
	raw, err := os.ReadFile(existing)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(raw), "# stale") {
		t.Errorf("--force should have replaced the stale file; still contains '# stale'")
	}
	if !strings.Contains(string(raw), "active_profile: cloud-only") {
		t.Errorf("--force overwrite should land on active_profile=cloud-only; got:\n%s", firstNLines(string(raw), 5))
	}
}

func firstNLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
