package installpkg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestInstallConfigShipsExpectedFiles is the install-time
// regression guard: every file aidev's runtime expects must be
// present in the embedded fileset that `aidev install` lays down.
//
// In v0.5 the shipped set is exactly two files — models.yaml
// (which now contains all profiles, no per-preset files) and
// principles.yaml. Adding or removing a file to the embedded set
// should be a deliberate change to this test.
func TestInstallConfigShipsExpectedFiles(t *testing.T) {
	dir := t.TempDir()
	written, _, err := InstallConfig(dir, true)
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]bool{
		"models.yaml":     false,
		"principles.yaml": false,
	}
	// Track files we DIDN'T expect so the legacy preset cleanup
	// (models.local.yaml, models.local-mixed.yaml) doesn't sneak
	// back via a stray copy in internal/installpkg/files/.
	var unexpected []string
	for _, p := range written {
		name := filepath.Base(p)
		if _, tracked := want[name]; tracked {
			want[name] = true
		} else {
			unexpected = append(unexpected, name)
		}
	}
	for name, saw := range want {
		if !saw {
			t.Errorf("InstallConfig did not write %q — did you forget to copy it into internal/installpkg/files/?", name)
		}
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("expected %s on disk after install: %v", name, err)
		}
	}
	if len(unexpected) > 0 {
		t.Errorf("InstallConfig wrote unexpected files: %v — v0.5 ships only models.yaml + principles.yaml. Remove stragglers from internal/installpkg/files/.", unexpected)
	}
}

// TestCloudOnlyProfileHasNoOllamaRoutes verifies the shipped
// cloud-only profile routes every tier through a non-ollama
// provider. This is the load-bearing contract of the profile
// (issue #56) — a user without Ollama installed must be able to
// `aidev config use-profile cloud-only` and have everything work.
// Any regression that reintroduces an ollama-backed tier to this
// profile breaks that promise.
func TestCloudOnlyProfileHasNoOllamaRoutes(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := InstallConfig(dir, true); err != nil {
		t.Fatalf("InstallConfig: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "models.yaml"))
	if err != nil {
		t.Fatalf("read models.yaml: %v", err)
	}

	// Minimal decode — we only need tier providers from one profile
	// to enforce the no-ollama invariant. Full config parsing lives
	// in internal/config.
	type tier struct {
		Provider string `yaml:"provider"`
	}
	type profile struct {
		Description string          `yaml:"description"`
		Tiers       map[string]tier `yaml:"tiers"`
		Routing     map[string]string `yaml:"routing"`
	}
	type root struct {
		Profiles map[string]profile `yaml:"profiles"`
	}
	var r root
	if err := yaml.Unmarshal(raw, &r); err != nil {
		t.Fatalf("unmarshal models.yaml: %v", err)
	}
	p, ok := r.Profiles["cloud-only"]
	if !ok {
		available := make([]string, 0, len(r.Profiles))
		for name := range r.Profiles {
			available = append(available, name)
		}
		t.Fatalf("cloud-only profile not present in shipped models.yaml — did you forget to add it?\nAvailable: %v", available)
	}
	if len(p.Tiers) == 0 {
		t.Fatal("cloud-only profile has no tiers")
	}
	for name, tr := range p.Tiers {
		if tr.Provider == "ollama" {
			t.Errorf("cloud-only tier %q routes to provider=ollama — breaks the no-Ollama promise", name)
		}
	}
	// Every routed role must point at a tier that exists in the
	// profile — guards against a typo like `routing: critic: claud_large`
	// after a rename.
	for role, tierName := range p.Routing {
		if _, ok := p.Tiers[tierName]; !ok {
			t.Errorf("cloud-only role %q → tier %q does not exist in profile.Tiers", role, tierName)
		}
	}
	// Sanity: description should mention \"no Ollama\" or \"claude\"
	// so anyone reading the file understands the profile's purpose.
	low := strings.ToLower(p.Description)
	if !strings.Contains(low, "ollama") && !strings.Contains(low, "claude") {
		t.Errorf("cloud-only description should explain the no-Ollama / claude-cli tradeoff; got: %q", p.Description)
	}
}

