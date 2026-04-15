package main

// Tests for the byte-level YAML mutation helpers that back
// `aidev config profile use` and `aidev config set`.
//
// The critical invariants we need to lock down:
//
//   - A profile-use round-trip (default → offline → default) must
//     produce a byte-for-byte identical file. No whitespace drift,
//     no comment loss.
//   - A set round-trip must also be byte-identical.
//   - Mutations validate the file post-write and roll back from
//     the .bak copy on validation failure.
//   - The line-locator finds the right line even when the file has
//     comments and blank lines in between.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sampleModelsYAML = `# preamble comment
# more preamble

active_profile: default

profiles:

  default:
    description: |
      The default test profile.
    tiers:
      small:
        provider: ollama
        model: qwen2.5-coder:7b
        endpoint: http://localhost:11434
        max_tokens: 2048
        temperature: 0.2
      cloud_large:
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
      coordinator: cloud_large

  offline:
    description: |
      Fully offline test profile.
    tiers:
      bare:
        provider: ollama
        model: qwen2.5-coder:14b
        endpoint: http://localhost:11434
        max_tokens: 4096
        temperature: 0.1
    routing:
      scout: bare
      tester: bare
      implementer: bare
      reviewer: bare
      critic: bare
      architect: bare
      charter: bare
      coordinator: bare
`

const principlesTestYAML = `principles:
  - name: Test
    summary: For tests
    description: Just a test
`

func setupTestConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "models.yaml"), []byte(sampleModelsYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "principles.yaml"), []byte(principlesTestYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestWriteActiveProfilePreservesFile is the headline guarantee:
// switching profiles and switching back produces a byte-identical
// file. If yaml.Marshal sneaks back into the path this test fails
// immediately.
func TestWriteActiveProfilePreservesFile(t *testing.T) {
	dir := setupTestConfig(t)
	original, err := os.ReadFile(filepath.Join(dir, "models.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	if err := writeActiveProfile(dir, "offline"); err != nil {
		t.Fatalf("switch to offline: %v", err)
	}
	switched, err := os.ReadFile(filepath.Join(dir, "models.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(switched), "active_profile: offline") {
		t.Errorf("active_profile not switched; got:\n%s", switched)
	}
	// Every other line should be unchanged.
	if !blockedDiffOnlyOnLine(original, switched, "active_profile:") {
		t.Errorf("switch to offline modified more than the active_profile line.\noriginal:\n%s\nswitched:\n%s", original, switched)
	}

	if err := writeActiveProfile(dir, "default"); err != nil {
		t.Fatalf("switch back to default: %v", err)
	}
	roundtrip, err := os.ReadFile(filepath.Join(dir, "models.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, roundtrip) {
		t.Errorf("round-trip changed bytes.\nbefore (%d bytes):\n%s\nafter (%d bytes):\n%s",
			len(original), original, len(roundtrip), roundtrip)
	}
}

// TestWriteTierProviderPreservesFile is the same guarantee for
// `aidev config set` — tweaking a tier's provider+model and
// reverting must round-trip byte-for-byte.
func TestWriteTierProviderPreservesFile(t *testing.T) {
	dir := setupTestConfig(t)
	original, err := os.ReadFile(filepath.Join(dir, "models.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	// Tweak the small tier from qwen2.5-coder:7b to qwen2.5-coder:3b.
	if err := writeTierProvider(dir, "default", "small", "ollama", "qwen2.5-coder:3b"); err != nil {
		t.Fatalf("set: %v", err)
	}
	tweaked, err := os.ReadFile(filepath.Join(dir, "models.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(tweaked), "model: qwen2.5-coder:3b") {
		t.Errorf("tier model not updated; got:\n%s", tweaked)
	}

	// Revert.
	if err := writeTierProvider(dir, "default", "small", "ollama", "qwen2.5-coder:7b"); err != nil {
		t.Fatalf("revert: %v", err)
	}
	roundtrip, err := os.ReadFile(filepath.Join(dir, "models.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, roundtrip) {
		t.Errorf("round-trip changed bytes.\nbefore len=%d:\n%s\nafter len=%d:\n%s",
			len(original), original, len(roundtrip), roundtrip)
	}
}

// TestWriteActiveProfileRollsBackOnValidationFailure verifies
// the safety net: writing an invalid profile name (one that
// doesn't exist) must NOT leave the file in a broken state. The
// .bak rollback is what protects users from a fat-finger.
func TestWriteActiveProfileRollsBackOnValidationFailure(t *testing.T) {
	dir := setupTestConfig(t)
	original, err := os.ReadFile(filepath.Join(dir, "models.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	err = writeActiveProfile(dir, "nonexistent")
	if err == nil {
		t.Fatal("expected validation error for nonexistent profile")
	}

	after, err := os.ReadFile(filepath.Join(dir, "models.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, after) {
		t.Errorf("file should have been rolled back on validation failure.\nbefore:\n%s\nafter:\n%s", original, after)
	}
}

// TestWriteTierProviderRollsBackOnValidationFailure verifies the
// same rollback for `set` mutations.
func TestWriteTierProviderRollsBackOnValidationFailure(t *testing.T) {
	dir := setupTestConfig(t)
	original, err := os.ReadFile(filepath.Join(dir, "models.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	// Setting a tier's provider to an empty string should fail
	// validation in config.Load (validateProfile rejects empty
	// providers).
	err = writeTierProvider(dir, "default", "small", "", "qwen2.5-coder:7b")
	if err == nil {
		t.Fatal("expected validation error for empty provider")
	}

	after, err := os.ReadFile(filepath.Join(dir, "models.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, after) {
		t.Errorf("file should have been rolled back; got divergence")
	}
}

// TestNodeLineForPathFindsNestedField verifies the line locator
// resolves a deep mapping path against a YAML doc with comments
// and blank lines interleaved. Catches off-by-one errors in the
// node walk.
func TestNodeLineForPathFindsNestedField(t *testing.T) {
	data := []byte(sampleModelsYAML)
	line, err := nodeLineForPath(data, []string{"profiles", "default", "tiers", "small", "model"})
	if err != nil {
		t.Fatal(err)
	}
	// Verify the line we found actually contains "model:" and
	// the right value. This is a stronger assertion than line
	// number alone because the test will keep working if we
	// reformat sampleModelsYAML.
	lines := strings.Split(string(data), "\n")
	if line < 1 || line > len(lines) {
		t.Fatalf("line %d out of range", line)
	}
	got := strings.TrimSpace(lines[line-1])
	if !strings.HasPrefix(got, "model:") {
		t.Errorf("line %d = %q, want starts with 'model:'", line, got)
	}
	if !strings.Contains(got, "qwen2.5-coder:7b") {
		t.Errorf("line %d = %q, want the small tier's model", line, got)
	}
}

// TestNodeLineForPathFailsOnUnknownKey verifies the error case.
func TestNodeLineForPathFailsOnUnknownKey(t *testing.T) {
	data := []byte(sampleModelsYAML)
	_, err := nodeLineForPath(data, []string{"profiles", "missing", "tiers", "small", "model"})
	if err == nil {
		t.Error("expected error for unknown profile key")
	}
}

// TestReplaceScalarOnLineRejectsKeyMismatch is the safety check
// that prevents a stale line number from corrupting a different
// field. If the caller's line number points at a "provider: ..."
// line but they pass key="model", we must refuse the edit
// rather than write to the wrong field.
func TestReplaceScalarOnLineRejectsKeyMismatch(t *testing.T) {
	data := []byte(`active_profile: default
provider: ollama
model: foo
`)
	_, err := replaceScalarOnLine(data, 2, "model", "bar")
	if err == nil {
		t.Error("expected error: line 2 has key 'provider', not 'model'")
	}
}

// blockedDiffOnlyOnLine is a tiny helper: returns true if the
// only line difference between a and b is a line that contains
// the given marker. Used to assert that switching active_profile
// touched exactly one line and nothing else.
func blockedDiffOnlyOnLine(a, b []byte, marker string) bool {
	la := strings.Split(string(a), "\n")
	lb := strings.Split(string(b), "\n")
	if len(la) != len(lb) {
		return false
	}
	for i := range la {
		if la[i] == lb[i] {
			continue
		}
		if !strings.Contains(la[i], marker) || !strings.Contains(lb[i], marker) {
			return false
		}
	}
	return true
}
