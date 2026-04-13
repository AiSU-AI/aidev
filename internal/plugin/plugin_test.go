package plugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallWritesAllCommandsToEmptyDir(t *testing.T) {
	dir := t.TempDir()
	written, skipped, err := Install(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(written) == 0 {
		t.Fatal("expected at least one file to be written")
	}
	if len(skipped) != 0 {
		t.Errorf("empty dir should have zero skipped, got %d", len(skipped))
	}
	// Every written file should exist on disk and start with YAML
	// frontmatter (the Claude Code slash command format).
	for _, p := range written {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Errorf("written file unreadable: %v", err)
			continue
		}
		if !strings.HasPrefix(string(data), "---\n") {
			t.Errorf("file %s does not start with YAML frontmatter", p)
		}
	}
}

func TestInstallSkipsExistingFilesWithoutForce(t *testing.T) {
	dir := t.TempDir()
	// Pre-create one of the command files with bogus content.
	marker := "USER EDITED THIS\n"
	existing := filepath.Join(dir, "aidev-run.md")
	if err := os.WriteFile(existing, []byte(marker), 0o644); err != nil {
		t.Fatal(err)
	}

	written, skipped, err := Install(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) == 0 {
		t.Errorf("expected at least one skipped file")
	}
	// The pre-existing file should still have its original content.
	data, _ := os.ReadFile(existing)
	if string(data) != marker {
		t.Errorf("pre-existing file was overwritten: %q", data)
	}
	// Some files should have been written (the non-conflicting ones).
	if len(written) == 0 {
		t.Error("expected non-conflicting files to be installed")
	}
}

func TestInstallForceOverwritesExistingFiles(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "aidev-run.md")
	if err := os.WriteFile(existing, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, skipped, err := Install(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 0 {
		t.Errorf("force should skip nothing, got %d", len(skipped))
	}
	data, _ := os.ReadFile(existing)
	if string(data) == "old" {
		t.Error("--force did not overwrite the existing file")
	}
}

func TestUninstallRemovesShippedFilesOnly(t *testing.T) {
	dir := t.TempDir()
	// Install first.
	if _, _, err := Install(dir, false); err != nil {
		t.Fatal(err)
	}
	// Replace one of the installed files with user content so it
	// should NOT be removed by uninstall.
	modified := filepath.Join(dir, "aidev-run.md")
	if err := os.WriteFile(modified, []byte("user modified this\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	removed, err := Uninstall(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) == 0 {
		t.Error("expected at least one file to be removed")
	}
	// The modified file should still exist untouched.
	data, err := os.ReadFile(modified)
	if err != nil {
		t.Fatalf("modified file was removed: %v", err)
	}
	if string(data) != "user modified this\n" {
		t.Errorf("modified file was overwritten: %q", data)
	}
}

func TestDefaultCommandsDirHonoursClaudeConfigDir(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "/tmp/fake-claude-config")
	got, err := DefaultCommandsDir()
	if err != nil {
		t.Fatal(err)
	}
	if got != "/tmp/fake-claude-config/commands" {
		t.Errorf("got %q, want /tmp/fake-claude-config/commands", got)
	}
}

func TestDefaultCommandsDirFallsBackToHome(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	got, err := DefaultCommandsDir()
	if err != nil {
		t.Fatal(err)
	}
	// Should end with /.claude/commands regardless of host OS
	if !strings.HasSuffix(got, filepath.Join(".claude", "commands")) {
		t.Errorf("got %q, should end with .claude/commands", got)
	}
}

func TestInstallEmptyTargetDirErrors(t *testing.T) {
	_, _, err := Install("", false)
	if err == nil {
		t.Error("expected error on empty target dir")
	}
}
