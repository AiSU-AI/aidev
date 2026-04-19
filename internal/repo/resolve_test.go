package repo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveLocalPath_HappyPath covers the common case: a legitimate
// local directory resolves cleanly, no URL-shape enrichment fires.
func TestResolveLocalPath_HappyPath(t *testing.T) {
	dir := t.TempDir()
	abs, err := ResolveLocalPath(dir)
	if err != nil {
		t.Fatalf("expected nil error for existing dir, got %v", err)
	}
	expected, _ := filepath.Abs(dir)
	if abs != expected {
		t.Errorf("expected resolved path %q, got %q", expected, abs)
	}
}

// TestResolveLocalPath_FileNotDir covers the edge case of `-repo` pointing
// at a regular file: must fail with "not a directory", not silently
// accept.
func TestResolveLocalPath_FileNotDir(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "some-file.txt")
	if err := os.WriteFile(filePath, []byte("x"), 0o644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	_, err := ResolveLocalPath(filePath)
	if err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("expected 'not a directory' error, got %v", err)
	}
}

// TestResolveLocalPath_URLShapes exercises the three URL shapes (plus
// variations on suffix/host) that Sketch 3 must detect when the raw
// input doesn't resolve to a real local directory. Each case verifies:
//
//   - ResolveLocalPath returns a non-nil error
//   - The error message mentions 'gh repo clone'
//   - The error message names the parsed owner + repo correctly
func TestResolveLocalPath_URLShapes(t *testing.T) {
	cases := []struct {
		name      string
		input     string
		wantOwner string
		wantRepo  string
	}{
		{"https plain", "https://github.com/AiSU-AI/aidev", "AiSU-AI", "aidev"},
		{"https with .git", "https://github.com/AiSU-AI/aidev.git", "AiSU-AI", "aidev"},
		{"https with trailing slash", "https://github.com/AiSU-AI/aidev/", "AiSU-AI", "aidev"},
		{"http non-tls", "http://example.com/owner/repo", "owner", "repo"},
		{"ssh plain", "git@github.com:AiSU-AI/aidev", "AiSU-AI", "aidev"},
		{"ssh with .git", "git@github.com:AiSU-AI/aidev.git", "AiSU-AI", "aidev"},
		{"ssh custom host", "git@gitlab.company.com:team/project.git", "team", "project"},
		{"bare shorthand", "AiSU-AI/aidev", "AiSU-AI", "aidev"},
		{"bare shorthand with .git", "AiSU-AI/aidev.git", "AiSU-AI", "aidev"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Use a cwd that definitely won't match the raw input
			// path (the temp dir has no subdir named after any of
			// these cases). That forces the code through the
			// stat-failure branch and into URL-shape detection.
			_, err := ResolveLocalPath(c.input)
			if err == nil {
				t.Fatalf("expected error for URL-shaped input %q, got nil", c.input)
			}
			msg := err.Error()
			if !strings.Contains(msg, "gh repo clone") {
				t.Errorf("expected error to mention 'gh repo clone', got: %s", msg)
			}
			wantSlug := c.wantOwner + "/" + c.wantRepo
			if !strings.Contains(msg, wantSlug) {
				t.Errorf("expected error to mention %q, got: %s", wantSlug, msg)
			}
		})
	}
}

// TestResolveLocalPath_ShorthandCollidesWithRealDir is the pathological
// case the Critic's sharp question #1 flagged: a user passes
// `-repo owner/repo` and `./owner/repo` actually exists as a real local
// directory. Sketch 3's stat-first ordering says the real directory
// wins; URL-shape detection never fires.
//
// This is the behaviour the choice of Sketch 3 over Sketch 1 was
// meant to protect — encode it as an explicit regression test.
func TestResolveLocalPath_ShorthandCollidesWithRealDir(t *testing.T) {
	// Build a temp tree that contains a literal `foo/bar` subdir and
	// chdir into it so the relative path "foo/bar" resolves to a real
	// directory.
	root := t.TempDir()
	nested := filepath.Join(root, "foo", "bar")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	origCwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(origCwd) })

	abs, err := ResolveLocalPath("foo/bar")
	if err != nil {
		t.Fatalf("expected ResolveLocalPath to accept a real local %q, got error: %v", "foo/bar", err)
	}
	wantSuffix := filepath.Join("foo", "bar")
	if !strings.HasSuffix(abs, wantSuffix) {
		t.Errorf("expected resolved path to end with %q, got %q", wantSuffix, abs)
	}
}

// TestResolveLocalPath_UnambiguousLocalPath confirms a dotted or
// dot-slash-prefixed relative path never triggers URL-shape detection
// (there's no slash in a way that looks like owner/repo, or it starts
// with `./`).
func TestResolveLocalPath_UnambiguousLocalPath(t *testing.T) {
	root := t.TempDir()
	origCwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(origCwd) })

	cases := []string{"./nonexistent-dir", "../nonexistent-dir"}
	for _, p := range cases {
		t.Run(p, func(t *testing.T) {
			_, err := ResolveLocalPath(p)
			if err == nil {
				t.Fatalf("expected error for missing path, got nil")
			}
			if strings.Contains(err.Error(), "gh repo clone") {
				t.Errorf("URL-shape enrichment should NOT fire for %q; got: %s", p, err.Error())
			}
		})
	}
}

// TestDetectURLShape_NegativeCases guards against false-positive
// detections on strings that look plausibly path-like but are not
// intended as URL shorthand.
func TestDetectURLShape_NegativeCases(t *testing.T) {
	cases := []string{
		"",
		"just-one-segment",
		"/absolute/path/segments",
		"three/segments/deep",
		"contains spaces/here",
		"owner/repo/extra",
	}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			owner, repo, ok := detectURLShape(c)
			if ok {
				t.Errorf("expected detectURLShape(%q) to return false; got owner=%q repo=%q", c, owner, repo)
			}
		})
	}
}
