package agents

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseFilesFromDiff(t *testing.T) {
	diff := `diff --git a/cmd/main.go b/cmd/main.go
--- a/cmd/main.go
+++ b/cmd/main.go
@@ -1,3 +1,4 @@
 package main
+import "fmt"

diff --git a/internal/foo.go b/internal/foo.go
--- /dev/null
+++ b/internal/foo.go
@@ -0,0 +1,3 @@
+package internal
+

diff --git a/deleted.go b/deleted.go
--- a/deleted.go
+++ /dev/null
`
	got := parseFilesFromDiff(diff)
	want := []string{"cmd/main.go", "internal/foo.go"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("got[%d] = %q, want %q", i, got[i], w)
		}
	}
}

func TestParseFilesFromDiffSkipsDevNullTargets(t *testing.T) {
	diff := `diff --git a/x.go b/x.go
--- a/x.go
+++ /dev/null
`
	got := parseFilesFromDiff(diff)
	if len(got) != 0 {
		t.Errorf("should not have recorded deleted file, got %v", got)
	}
}

func TestStripCodeFenceRemovesMarkdownWrap(t *testing.T) {
	wrapped := "```diff\ndiff --git a/x b/x\n@@ -1 +1 @@\n-a\n+b\n```"
	got := stripCodeFence(wrapped)
	if strings.HasPrefix(got, "```") || strings.HasSuffix(got, "```") {
		t.Errorf("fences not removed: %q", got)
	}
	if !strings.HasPrefix(got, "diff --git") {
		t.Errorf("diff content corrupted: %q", got)
	}
}

func TestStripCodeFenceLeavesUnwrappedAlone(t *testing.T) {
	raw := "diff --git a/x b/x\n@@ -1 +1 @@\n-a\n+b"
	if got := stripCodeFence(raw); got != raw {
		t.Errorf("unwrapped content should be unchanged, got %q", got)
	}
}

func TestPatchWriteToCreatesAidevDirAndFile(t *testing.T) {
	dir := t.TempDir()
	p := &Patch{Diff: "diff --git a/x b/x\n@@ -1 +1 @@\n-a\n+b"}
	path, err := p.WriteTo(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, ".aidev", "proposed.patch")
	if path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	if p.Path != path {
		t.Errorf("p.Path not updated: %q", p.Path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "diff --git") {
		t.Error("patch file missing diff")
	}
	if !strings.Contains(string(data), "Written by aidev") {
		t.Error("patch file missing footer stamp")
	}
}

func TestListSourceFilesRespectsLimit(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 20; i++ {
		name := filepath.Join(dir, "file"+intoa(i)+".go")
		if err := os.WriteFile(name, []byte("package x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := listSourceFiles(dir, 5)
	if len(got) != 5 {
		t.Errorf("got %d files, want 5 (limit)", len(got))
	}
}

func TestListSourceFilesSkipsVendorDirs(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "node_modules", "foo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "node_modules", "foo", "x.js"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := listSourceFiles(dir, 100)
	for _, f := range got {
		if strings.Contains(f, "node_modules") {
			t.Errorf("should have skipped node_modules: %q", f)
		}
	}
	if len(got) != 1 || got[0] != "main.go" {
		t.Errorf("got %v, want [main.go]", got)
	}
}

func TestListSourceFilesSkipsNonSourceExtensions(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "image.png"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := listSourceFiles(dir, 100)
	if len(got) != 1 || got[0] != "main.go" {
		t.Errorf("got %v, want [main.go]", got)
	}
}

func intoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
