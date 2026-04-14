package installpkg

import (
	"os"
	"path/filepath"
	"testing"
)

// TestInstallConfigShipsLocalPreset verifies that the install-time
// fileset includes models.local.yaml alongside models.yaml and
// principles.yaml. Without this, users running `aidev install` on a
// fresh machine would get the default tier routing but not the
// preset file, so `--preset local` would fail with a missing-file
// error — a silent-first-use footgun.
func TestInstallConfigShipsLocalPreset(t *testing.T) {
	dir := t.TempDir()
	written, _, err := InstallConfig(dir, true)
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]bool{
		"models.yaml":       false,
		"models.local.yaml": false,
		"principles.yaml":   false,
	}
	for _, p := range written {
		name := filepath.Base(p)
		if _, tracked := want[name]; tracked {
			want[name] = true
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
}
