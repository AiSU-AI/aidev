package repo

import (
	"os"
	"path/filepath"
	"strings"
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

func TestScanBuildsDepth2DirectoryTree(t *testing.T) {
	dir := t.TempDir()

	// Build a monorepo-ish layout. The fix guards against Scout describing
	// "apps/web, apps/api" when only "apps/marketing" exists, so this
	// fixture exercises the real failure mode.
	for _, sub := range []string{"apps/marketing/src", "packages/shared/lib", "node_modules/junk/deep"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(dir, "package.json"), "{}")
	writeFile(t, filepath.Join(dir, "apps/marketing/src/app.tsx"), "x")
	writeFile(t, filepath.Join(dir, "node_modules/junk/deep/ignored.js"), "x")

	snap, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	tree := snap.DirectoryTree
	if tree == "" {
		t.Fatal("DirectoryTree is empty")
	}
	// Must show the real second-level directories.
	for _, want := range []string{"apps/", "marketing/", "packages/", "shared/", "package.json"} {
		if !strings.Contains(tree, want) {
			t.Errorf("DirectoryTree missing %q; full tree:\n%s", want, tree)
		}
	}
	// Must NOT leak hallucination-candidate dirs (they literally don't exist).
	for _, bad := range []string{"web/", "api/"} {
		if strings.Contains(tree, bad) {
			t.Errorf("DirectoryTree unexpectedly contains %q; full tree:\n%s", bad, tree)
		}
	}
	// node_modules must be pruned at its root, not just the deep leaf.
	if strings.Contains(tree, "node_modules") {
		t.Errorf("DirectoryTree must skip node_modules; full tree:\n%s", tree)
	}
	// Depth-2 only: files inside depth-2 dirs are not listed.
	if strings.Contains(tree, "app.tsx") {
		t.Errorf("DirectoryTree should not list depth-3 files; full tree:\n%s", tree)
	}
}

func TestRenderDirectoryTreeRespectsMaxBytes(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 50; i++ {
		if err := os.MkdirAll(filepath.Join(dir, "d"+string(rune('a'+i%26))+string(rune('a'+(i/26)%26))), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	skip := map[string]bool{".git": true}
	out := renderDirectoryTree(dir, skip, 2, 1000, 64) // tiny byte cap
	if len(out) > 64+len("... [truncated]\n") {
		t.Errorf("renderDirectoryTree exceeded byte cap: len=%d output=%q", len(out), out)
	}
	if !strings.Contains(out, "truncated") {
		t.Errorf("expected truncation marker, got: %q", out)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
