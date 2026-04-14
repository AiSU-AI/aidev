package agents

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aisu-ai/aidev/internal/llm"
)

func setupRepo(t *testing.T) (string, *ToolExecutor) {
	t.Helper()
	dir := t.TempDir()
	// Build a tiny marketing-monorepo-shaped tree so the tests
	// exercise realistic paths (nested components, multiple locales,
	// mixed extensions).
	must := func(path, body string) {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	must("apps/marketing/src/components/pages/pricing/pricing.tsx",
		"import { t } from '@/lib/i18n'\nexport const Pricing = () => <>{t('contactSales')}</>\n")
	must("apps/marketing/src/components/pages/solutions/solutions.tsx",
		"export const Solutions = () => <>hello</>\n")
	must("apps/marketing/src/lib/i18n/translations/en/common.json",
		`{"contactSales":"Contact Sales"}`+"\n")
	must("apps/marketing/src/lib/i18n/translations/fr/common.json",
		`{"contactSales":"Contacter les ventes"}`+"\n")
	must("apps/marketing/src/lib/i18n/translations/de/common.json",
		`{"contactSales":"Vertrieb kontaktieren"}`+"\n")
	must("apps/backend/main.go", "package main\n")
	must("node_modules/should-be-skipped/index.js", "blocked")
	must("README.md", "# test repo\n")
	return dir, NewToolExecutor(dir)
}

func callTool(name string, input string) llm.ToolUse {
	return llm.ToolUse{
		ID:    "test_" + name,
		Name:  name,
		Input: json.RawMessage(input),
	}
}

func TestReadFileTool(t *testing.T) {
	_, exec := setupRepo(t)
	out := exec.Execute(callTool("read_file", `{"path":"apps/marketing/src/lib/i18n/translations/en/common.json"}`))
	if out.ToolResultIsError {
		t.Fatalf("unexpected error: %s", out.ToolResultContent)
	}
	if !strings.Contains(out.ToolResultContent, "Contact Sales") {
		t.Errorf("expected content, got: %s", out.ToolResultContent)
	}
}

func TestReadFileToolNotFound(t *testing.T) {
	_, exec := setupRepo(t)
	out := exec.Execute(callTool("read_file", `{"path":"does/not/exist.tsx"}`))
	if !out.ToolResultIsError {
		t.Error("expected IsError=true for missing file")
	}
	if !strings.Contains(out.ToolResultContent, "file not found") {
		t.Errorf("error message should be clear, got: %s", out.ToolResultContent)
	}
}

func TestReadFileToolRejectsTraversal(t *testing.T) {
	_, exec := setupRepo(t)
	out := exec.Execute(callTool("read_file", `{"path":"../etc/passwd"}`))
	if !out.ToolResultIsError {
		t.Error("traversal path should be rejected")
	}
}

func TestReadFileToolRejectsAbsolute(t *testing.T) {
	_, exec := setupRepo(t)
	out := exec.Execute(callTool("read_file", `{"path":"/etc/passwd"}`))
	if !out.ToolResultIsError {
		t.Error("absolute path should be rejected")
	}
}

func TestReadFileToolTruncatesLargeFiles(t *testing.T) {
	dir, exec := setupRepo(t)
	exec.MaxReadBytes = 50
	big := strings.Repeat("x", 500)
	if err := os.WriteFile(filepath.Join(dir, "big.txt"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	out := exec.Execute(callTool("read_file", `{"path":"big.txt"}`))
	if out.ToolResultIsError {
		t.Fatal("should succeed with truncation, not error")
	}
	if !strings.Contains(out.ToolResultContent, "[truncated by aidev") {
		t.Errorf("missing truncation marker; got:\n%s", out.ToolResultContent)
	}
	if len(out.ToolResultContent) > 200 {
		t.Errorf("truncated output too long: %d bytes", len(out.ToolResultContent))
	}
}

func TestGlobToolFindsNestedTSX(t *testing.T) {
	_, exec := setupRepo(t)
	out := exec.Execute(callTool("glob", `{"pattern":"apps/marketing/**/*.tsx"}`))
	if out.ToolResultIsError {
		t.Fatalf("glob failed: %s", out.ToolResultContent)
	}
	for _, want := range []string{
		"apps/marketing/src/components/pages/pricing/pricing.tsx",
		"apps/marketing/src/components/pages/solutions/solutions.tsx",
	} {
		if !strings.Contains(out.ToolResultContent, want) {
			t.Errorf("glob missing %s; got:\n%s", want, out.ToolResultContent)
		}
	}
}

func TestGlobToolFindsLocaleJSONs(t *testing.T) {
	_, exec := setupRepo(t)
	out := exec.Execute(callTool("glob", `{"pattern":"**/common.json"}`))
	if out.ToolResultIsError {
		t.Fatalf("glob failed: %s", out.ToolResultContent)
	}
	count := strings.Count(out.ToolResultContent, "common.json")
	if count != 3 {
		t.Errorf("want 3 locale hits, got %d:\n%s", count, out.ToolResultContent)
	}
}

func TestGlobToolSkipsNodeModules(t *testing.T) {
	_, exec := setupRepo(t)
	out := exec.Execute(callTool("glob", `{"pattern":"**/*.js"}`))
	if strings.Contains(out.ToolResultContent, "node_modules") {
		t.Errorf("glob should skip node_modules; got:\n%s", out.ToolResultContent)
	}
}

func TestGlobToolNoMatches(t *testing.T) {
	_, exec := setupRepo(t)
	out := exec.Execute(callTool("glob", `{"pattern":"**/*.xyz"}`))
	if out.ToolResultIsError {
		t.Error("no-matches should not be an error")
	}
	if !strings.Contains(out.ToolResultContent, "no matches") {
		t.Errorf("expected 'no matches', got: %s", out.ToolResultContent)
	}
}

func TestGlobToolRejectsTraversal(t *testing.T) {
	_, exec := setupRepo(t)
	out := exec.Execute(callTool("glob", `{"pattern":"../etc/**"}`))
	if !out.ToolResultIsError {
		t.Error("traversal pattern should be rejected")
	}
}

func TestGrepToolFindsCallSite(t *testing.T) {
	_, exec := setupRepo(t)
	out := exec.Execute(callTool("grep", `{"pattern":"contactSales"}`))
	if out.ToolResultIsError {
		t.Fatalf("grep failed: %s", out.ToolResultContent)
	}
	// Should find it in both the TSX call site AND the JSON locale files.
	if !strings.Contains(out.ToolResultContent, "pricing.tsx") {
		t.Errorf("missing pricing.tsx hit; got:\n%s", out.ToolResultContent)
	}
	if !strings.Contains(out.ToolResultContent, "common.json") {
		t.Errorf("missing common.json hit; got:\n%s", out.ToolResultContent)
	}
}

func TestGrepToolScopedToSubdir(t *testing.T) {
	_, exec := setupRepo(t)
	out := exec.Execute(callTool("grep", `{"pattern":"contactSales","paths":["apps/marketing/src/lib"]}`))
	if out.ToolResultIsError {
		t.Fatalf("grep failed: %s", out.ToolResultContent)
	}
	// Only the locale JSONs are under apps/marketing/src/lib;
	// the TSX is under apps/marketing/src/components.
	if strings.Contains(out.ToolResultContent, "pricing.tsx") {
		t.Errorf("scoped grep should not have matched pricing.tsx; got:\n%s", out.ToolResultContent)
	}
	if !strings.Contains(out.ToolResultContent, "common.json") {
		t.Errorf("missing common.json hit; got:\n%s", out.ToolResultContent)
	}
}

func TestGrepToolInvalidRegex(t *testing.T) {
	_, exec := setupRepo(t)
	out := exec.Execute(callTool("grep", `{"pattern":"[[unclosed"}`))
	if !out.ToolResultIsError {
		t.Error("invalid regex should be reported as tool error")
	}
}

func TestGrepToolSkipsVendorDirs(t *testing.T) {
	_, exec := setupRepo(t)
	out := exec.Execute(callTool("grep", `{"pattern":"blocked"}`))
	if strings.Contains(out.ToolResultContent, "node_modules") {
		t.Errorf("grep should skip node_modules; got:\n%s", out.ToolResultContent)
	}
}

func TestListDirToolRoot(t *testing.T) {
	_, exec := setupRepo(t)
	out := exec.Execute(callTool("list_dir", `{"path":"."}`))
	if out.ToolResultIsError {
		t.Fatalf("list_dir failed: %s", out.ToolResultContent)
	}
	if !strings.Contains(out.ToolResultContent, "d/apps") {
		t.Errorf("missing d/apps; got:\n%s", out.ToolResultContent)
	}
	if !strings.Contains(out.ToolResultContent, "f/README.md") {
		t.Errorf("missing f/README.md; got:\n%s", out.ToolResultContent)
	}
	if strings.Contains(out.ToolResultContent, "node_modules") {
		t.Errorf("list_dir should skip node_modules; got:\n%s", out.ToolResultContent)
	}
}

func TestListDirToolSubdir(t *testing.T) {
	_, exec := setupRepo(t)
	out := exec.Execute(callTool("list_dir", `{"path":"apps/marketing/src/lib/i18n/translations"}`))
	if out.ToolResultIsError {
		t.Fatalf("list_dir failed: %s", out.ToolResultContent)
	}
	for _, want := range []string{"d/en", "d/fr", "d/de"} {
		if !strings.Contains(out.ToolResultContent, want) {
			t.Errorf("missing %s; got:\n%s", want, out.ToolResultContent)
		}
	}
}

func TestListDirToolRejectsNonDir(t *testing.T) {
	_, exec := setupRepo(t)
	out := exec.Execute(callTool("list_dir", `{"path":"README.md"}`))
	if !out.ToolResultIsError {
		t.Error("list_dir on a file should be an error")
	}
}

func TestUnknownToolIsRecoverableError(t *testing.T) {
	_, exec := setupRepo(t)
	out := exec.Execute(llm.ToolUse{ID: "x", Name: "eval", Input: json.RawMessage(`{}`)})
	if !out.ToolResultIsError {
		t.Error("unknown tool should be reported as tool error")
	}
	if !strings.Contains(out.ToolResultContent, "unknown tool") {
		t.Errorf("error message should be clear; got: %s", out.ToolResultContent)
	}
}

func TestDefinitionsIsStableAndWellFormed(t *testing.T) {
	_, exec := setupRepo(t)
	defs := exec.Definitions()
	if len(defs) != 4 {
		t.Fatalf("got %d tools, want 4", len(defs))
	}
	names := map[string]bool{}
	for _, d := range defs {
		if d.Name == "" {
			t.Errorf("tool missing Name")
		}
		if d.Description == "" {
			t.Errorf("tool %q missing Description", d.Name)
		}
		// InputSchema must be valid JSON with a top-level object.
		var schema map[string]any
		if err := json.Unmarshal(d.InputSchema, &schema); err != nil {
			t.Errorf("tool %q: InputSchema not valid JSON: %v", d.Name, err)
		}
		if schema["type"] != "object" {
			t.Errorf("tool %q: schema type should be object", d.Name)
		}
		names[d.Name] = true
	}
	for _, want := range []string{"read_file", "glob", "grep", "list_dir"} {
		if !names[want] {
			t.Errorf("missing tool definition: %s", want)
		}
	}
}

func TestGlobToRegexpDoublestar(t *testing.T) {
	cases := []struct {
		pattern string
		match   []string
		noMatch []string
	}{
		{
			pattern: "apps/marketing/**/*.tsx",
			match: []string{
				"apps/marketing/src/foo.tsx",
				"apps/marketing/src/components/pages/pricing/pricing.tsx",
			},
			noMatch: []string{
				"apps/backend/foo.tsx",
				"apps/marketing/src/foo.ts",
			},
		},
		{
			pattern: "**/common.json",
			match: []string{
				"common.json",
				"apps/marketing/src/lib/i18n/translations/en/common.json",
			},
			noMatch: []string{
				"other.json",
			},
		},
		{
			pattern: "packages/types/src/*.ts",
			match: []string{
				"packages/types/src/i18n.ts",
			},
			noMatch: []string{
				"packages/types/src/nested/i18n.ts",
				"packages/types/src/i18n.tsx",
			},
		},
	}
	for _, c := range cases {
		re, err := globToRegexp(c.pattern)
		if err != nil {
			t.Fatalf("compile %q: %v", c.pattern, err)
		}
		for _, m := range c.match {
			if !re.MatchString(m) {
				t.Errorf("pattern %q should match %q", c.pattern, m)
			}
		}
		for _, m := range c.noMatch {
			if re.MatchString(m) {
				t.Errorf("pattern %q should NOT match %q", c.pattern, m)
			}
		}
	}
}
