// Package plugin owns the "install aidev's slash commands into the
// user's Claude Code config" side of v0.2d. The files themselves live
// under `plugin/aidev/commands/` in the repo and are embedded into the
// binary via go:embed so the installer works regardless of where the
// user invoked `aidev` from.
package plugin

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// commandFiles is the embedded filesystem containing every slash
// command shipped by aidev. The `//go:embed` directive walks the tree
// at compile time — adding a new file under `plugin/aidev/commands/`
// is all you need to do to ship a new slash command.
//
//go:embed all:files
var commandFiles embed.FS

// Install copies every embedded slash command into the target
// directory. Missing parent directories are created. Existing files
// are preserved unless force is true, in which case they are
// overwritten.
//
// Returns the list of files actually written (absolute paths) and a
// list of files skipped because they already existed (only populated
// when force is false).
func Install(targetDir string, force bool) (written, skipped []string, err error) {
	if targetDir == "" {
		return nil, nil, errors.New("plugin install: empty target dir")
	}
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return nil, nil, fmt.Errorf("plugin install: mkdir %s: %w", targetDir, err)
	}

	entries, err := fs.ReadDir(commandFiles, "files")
	if err != nil {
		return nil, nil, fmt.Errorf("plugin install: read embedded files: %w", err)
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".md") {
			continue
		}
		data, err := fs.ReadFile(commandFiles, "files/"+name)
		if err != nil {
			return written, skipped, fmt.Errorf("plugin install: read %s: %w", name, err)
		}
		dest := filepath.Join(targetDir, name)
		if _, err := os.Stat(dest); err == nil && !force {
			skipped = append(skipped, dest)
			continue
		}
		if err := os.WriteFile(dest, data, 0o644); err != nil {
			return written, skipped, fmt.Errorf("plugin install: write %s: %w", dest, err)
		}
		written = append(written, dest)
	}
	return written, skipped, nil
}

// Uninstall removes every slash command aidev installed into the
// target directory. Files that match an embedded command name but
// whose content differs from the shipped version are preserved, so
// user edits are never destroyed.
//
// Returns the list of files actually removed.
func Uninstall(targetDir string) (removed []string, err error) {
	entries, err := fs.ReadDir(commandFiles, "files")
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".md") {
			continue
		}
		dest := filepath.Join(targetDir, name)
		shipped, err := fs.ReadFile(commandFiles, "files/"+name)
		if err != nil {
			continue
		}
		onDisk, err := os.ReadFile(dest)
		if err != nil {
			continue // not installed, nothing to remove
		}
		if !bytesEqual(shipped, onDisk) {
			// User edited the file — don't delete their work.
			continue
		}
		if err := os.Remove(dest); err != nil {
			return removed, fmt.Errorf("plugin uninstall: remove %s: %w", dest, err)
		}
		removed = append(removed, dest)
	}
	return removed, nil
}

// DefaultCommandsDir returns the directory where Claude Code looks for
// user-defined slash commands. Honours CLAUDE_CONFIG_DIR when set,
// otherwise falls back to ~/.claude/commands.
func DefaultCommandsDir() (string, error) {
	if v := os.Getenv("CLAUDE_CONFIG_DIR"); v != "" {
		return filepath.Join(v, "commands"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("plugin: resolve home dir: %w", err)
	}
	return filepath.Join(home, ".claude", "commands"), nil
}

// bytesEqual is a tiny helper so the package doesn't need to import
// the "bytes" package just for Equal.
func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
