package runpath

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunDirHonoursXDGDataHome(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmp)

	dir, err := RunDir("aisu-ai", "company-site", 533)
	if err != nil {
		t.Fatalf("RunDir: %v", err)
	}

	want := filepath.Join(tmp, "aidev", "runs", "aisu-ai-company-site-533")
	if dir != want {
		t.Errorf("dir = %q, want %q", dir, want)
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if !info.IsDir() {
		t.Errorf("path exists but is not a directory: %s", dir)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("perms = %o, want 0755", info.Mode().Perm())
	}
}

func TestRunDirFallsBackToHomeWhenXDGUnset(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("HOME", tmp)

	dir, err := RunDir("a", "b", 1)
	if err != nil {
		t.Fatalf("RunDir: %v", err)
	}
	want := filepath.Join(tmp, ".local", "share", "aidev", "runs", "a-b-1")
	if dir != want {
		t.Errorf("dir = %q, want %q", dir, want)
	}
}

func TestRunDirRejectsBadInputs(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	cases := []struct {
		name  string
		owner string
		repo  string
		issue int
	}{
		{"empty owner", "", "repo", 1},
		{"empty repo", "owner", "", 1},
		{"zero issue", "owner", "repo", 0},
		{"negative issue", "owner", "repo", -1},
		{"whitespace owner", "  ", "repo", 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := RunDir(c.owner, c.repo, c.issue); err == nil {
				t.Errorf("expected error for owner=%q repo=%q issue=%d", c.owner, c.repo, c.issue)
			}
		})
	}
}

func TestRunDirSlugSanitizesPathSeparators(t *testing.T) {
	// A hostile owner like "evil/../escape" must not break out of the
	// runs directory. Verify the sanitiser reduces it to safe chars.
	tmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmp)

	dir, err := RunDir("evil/escape", "repo", 1)
	if err != nil {
		t.Fatalf("RunDir: %v", err)
	}

	// dir must be inside tmp/aidev/runs/ — never escape via ..
	expectedRoot := filepath.Join(tmp, "aidev", "runs") + string(filepath.Separator)
	if !strings.HasPrefix(dir, expectedRoot) {
		t.Errorf("slug escaped runs dir: %s (root: %s)", dir, expectedRoot)
	}
	if strings.Contains(filepath.Base(dir), "/") {
		t.Errorf("slug retained path separator: %s", filepath.Base(dir))
	}
}

func TestDataRootHonoursXDG(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "/some/custom/path")
	root, err := DataRoot()
	if err != nil {
		t.Fatalf("DataRoot: %v", err)
	}
	if root != "/some/custom/path/aidev" {
		t.Errorf("root = %q, want /some/custom/path/aidev", root)
	}
}
