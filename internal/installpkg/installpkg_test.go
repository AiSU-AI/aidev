package installpkg

import (
	"os"
	"path/filepath"
	"testing"
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
