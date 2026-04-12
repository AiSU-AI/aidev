package repo

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScanCollectsReadmeAndLanguages(t *testing.T) {
	dir := t.TempDir()

	writeFile(t, filepath.Join(dir, "README.md"), "# Test repo\nHello.")
	writeFile(t, filepath.Join(dir, "main.go"), "package main")
	writeFile(t, filepath.Join(dir, "lib.go"), "package lib")
	writeFile(t, filepath.Join(dir, "docs.md"), "notes")

	// A skipped directory should not contribute to counts.
	if err := os.MkdirAll(filepath.Join(dir, "node_modules", "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "node_modules", "pkg", "ignored.js"), "x")

	snap, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	if snap.ReadmePath == "" {
		t.Error("README not detected")
	}
	if got := snap.Languages[".go"]; got != 2 {
		t.Errorf("go files = %d, want 2", got)
	}
	if got := snap.Languages[".md"]; got != 2 {
		t.Errorf("md files = %d, want 2", got)
	}
	if snap.Languages[".js"] != 0 {
		t.Errorf("node_modules should have been skipped")
	}
	if got := snap.TopLanguages(2); len(got) == 0 {
		t.Error("TopLanguages returned empty slice")
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
