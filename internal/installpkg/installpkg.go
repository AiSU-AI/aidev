// Package installpkg writes aidev's shipped default config files into
// the user's XDG config directory on first install. The files are
// embedded into the binary at compile time via go:embed so the
// installer works regardless of where the binary is run from.
//
// This mirrors the plugin installer's shape (see internal/plugin) —
// embedded files, idempotent install, never clobbers user edits. The
// two are intentionally separate packages because config files and
// slash-command files have different lifecycles and different
// destinations.
package installpkg

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// configFiles holds the shipped default config (models.yaml +
// principles.yaml) bundled into the binary. Adding a new file under
// `files/` ships a new default automatically.
//
//go:embed all:files
var configFiles embed.FS

// DefaultConfigDir returns the XDG config directory where aidev's
// config files should land on a fresh install. Honours
// $XDG_CONFIG_HOME when set, otherwise falls back to ~/.config/aidev.
func DefaultConfigDir() (string, error) {
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		return filepath.Join(v, "aidev"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("install: resolve home dir: %w", err)
	}
	return filepath.Join(home, ".config", "aidev"), nil
}

// InstallConfig copies every shipped default config file into
// targetDir. Missing parent directories are created. Existing files
// are preserved unless force is true.
//
// Returns the list of files written and the list skipped (because
// they already existed and force was false).
func InstallConfig(targetDir string, force bool) (written, skipped []string, err error) {
	if targetDir == "" {
		return nil, nil, errors.New("install: empty target dir")
	}
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return nil, nil, fmt.Errorf("install: mkdir %s: %w", targetDir, err)
	}

	entries, err := fs.ReadDir(configFiles, "files")
	if err != nil {
		return nil, nil, fmt.Errorf("install: read embedded files: %w", err)
	}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		data, err := fs.ReadFile(configFiles, "files/"+name)
		if err != nil {
			return written, skipped, fmt.Errorf("install: read %s: %w", name, err)
		}
		dest := filepath.Join(targetDir, name)
		if _, err := os.Stat(dest); err == nil && !force {
			skipped = append(skipped, dest)
			continue
		}
		if err := os.WriteFile(dest, data, 0o644); err != nil {
			return written, skipped, fmt.Errorf("install: write %s: %w", dest, err)
		}
		written = append(written, dest)
	}
	return written, skipped, nil
}
