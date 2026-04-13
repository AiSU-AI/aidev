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

// v0.3a.1 two-turn Implementer tests.

func TestParseFileSelectionBareJSON(t *testing.T) {
	got, err := parseFileSelection(`["cmd/main.go", "internal/foo.go"]`)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "cmd/main.go" || got[1] != "internal/foo.go" {
		t.Errorf("got %v", got)
	}
}

func TestParseFileSelectionWrappedInCodeFence(t *testing.T) {
	raw := "```json\n[\"a.go\", \"b.go\"]\n```"
	got, err := parseFileSelection(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("got %v", got)
	}
}

func TestParseFileSelectionWithSurroundingProse(t *testing.T) {
	raw := "Here are the files I need:\n\n[\"main.go\"]\n\nLet me know if you need more."
	got, err := parseFileSelection(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "main.go" {
		t.Errorf("got %v", got)
	}
}

func TestParseFileSelectionEmptyErrors(t *testing.T) {
	if _, err := parseFileSelection(""); err == nil {
		t.Error("expected error on empty input")
	}
}

func TestParseFileSelectionNoJSONErrors(t *testing.T) {
	if _, err := parseFileSelection("sorry, I can't help with that"); err == nil {
		t.Error("expected error when no JSON array is present")
	}
}

func TestCleanFileListDropsUnsafePaths(t *testing.T) {
	in := []string{
		"good/path.go",
		"/absolute/path.go",       // absolute — drop
		"../escape.go",            // escape — drop
		"nested/../evil.go",       // escape — drop
		"good/path.go",            // duplicate — drop
		"another.go",
	}
	got := cleanFileList(in)
	want := []string{"good/path.go", "another.go"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("got[%d] = %q, want %q", i, got[i], w)
		}
	}
}

func TestCleanFileListCapsLength(t *testing.T) {
	in := make([]string, 30)
	for j := range in {
		in[j] = "file" + intoa(j) + ".go"
	}
	got := cleanFileList(in)
	if len(got) != maxRequestedFiles {
		t.Errorf("got %d, want %d", len(got), maxRequestedFiles)
	}
}

func TestIsSafeRelPath(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"foo/bar.go", true},
		{"foo.go", true},
		{"", false},
		{"/abs/path", false},
		{"../escape", false},
		{"foo/../escape", false},
		{"foo/./ok.go", true},
		{strings.Repeat("a", 2000), false},
	}
	for _, c := range cases {
		got := isSafeRelPath(c.in)
		if got != c.want {
			t.Errorf("isSafeRelPath(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestReadFilesSkipsMissingAndDirs(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "exists.go"), []byte("package main"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := readFiles(dir, []string{"exists.go", "missing.go", "sub"})
	if len(got) != 1 {
		t.Errorf("got %d files, want 1", len(got))
	}
	if got["exists.go"] != "package main" {
		t.Errorf("content mismatch: %q", got["exists.go"])
	}
}

func TestReadFilesTruncatesLargeFiles(t *testing.T) {
	dir := t.TempDir()
	big := strings.Repeat("x", maxFileBytes*2)
	if err := os.WriteFile(filepath.Join(dir, "big.txt"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	got := readFiles(dir, []string{"big.txt"})
	content := got["big.txt"]
	if !strings.Contains(content, "truncated") {
		t.Errorf("expected truncation marker, got first 100 chars: %q", content[:100])
	}
	// Length should be bounded by the limit + marker.
	if len(content) > maxFileBytes+500 {
		t.Errorf("truncated content too long: %d bytes", len(content))
	}
}

func TestReadFilesRefusesUnsafePaths(t *testing.T) {
	dir := t.TempDir()
	// Create a file OUTSIDE the intended root that an escape path
	// could try to reach.
	parent := filepath.Dir(dir)
	secret := filepath.Join(parent, "secret.txt")
	if err := os.WriteFile(secret, []byte("top secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(secret)

	got := readFiles(dir, []string{"../" + filepath.Base(secret), "/etc/passwd"})
	if len(got) != 0 {
		t.Errorf("unsafe paths should have been rejected, got %v", got)
	}
}

func TestSortStrings(t *testing.T) {
	a := []string{"c", "a", "b"}
	sortStrings(a)
	if a[0] != "a" || a[1] != "b" || a[2] != "c" {
		t.Errorf("sortStrings result: %v", a)
	}
}
