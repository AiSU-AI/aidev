// Tool executors for the Implementer's tool-use loop (v0.4).
//
// These run server-side in aidev — the LLM emits a tool_use block,
// the agent's tool loop dispatches it here, and the return value
// becomes a tool_result block in the next LLM turn.
//
// All tools are READ-ONLY and repo-scoped via isSafeRelPath from
// implementer.go. The Implementer still produces a diff; it doesn't
// mutate files directly. Write access would be an interesting but
// separate design decision (and a much bigger safety footprint).
//
// Tools available:
//
//	read_file(path)         contents of one file, size-capped
//	glob(pattern)           doublestar glob matches under the repo root
//	grep(pattern, paths?)   ripgrep-style text search
//	list_dir(path)          directory listing
//
// Each tool's JSON Schema is declared next to its executor so the
// model gets accurate parameter descriptions in the system prompt.
package agents

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/aisu-ai/aidev/internal/llm"
)

// ToolExecutor binds a repo root and a set of tool implementations
// so the Implementer's tool loop can dispatch tool_use calls.
// Construct one per Implement() invocation — not shared across runs
// because the repo root is invocation-scoped.
type ToolExecutor struct {
	RepoRoot string

	// Limits. Exposed as fields so tests can override them.
	MaxReadBytes int // per read_file call
	MaxGlobHits  int // per glob call
	MaxGrepHits  int // per grep call
	MaxListHits  int // per list_dir call
}

// NewToolExecutor returns a ToolExecutor with production default
// limits. Every Implementer.Run() call constructs a fresh one.
func NewToolExecutor(repoRoot string) *ToolExecutor {
	return &ToolExecutor{
		RepoRoot:     repoRoot,
		MaxReadBytes: 128 * 1024,
		MaxGlobHits:  500,
		MaxGrepHits:  200,
		MaxListHits:  500,
	}
}

// Definitions returns the llm.ToolDefinition slice that drives the
// system prompt's tool schema. Order is stable so the model sees
// the same tool list on every turn.
func (e *ToolExecutor) Definitions() []llm.ToolDefinition {
	return []llm.ToolDefinition{
		{
			Name:        "read_file",
			Description: "Read a single file from the repository. Returns the file contents as a UTF-8 string. Paths are repo-relative (no leading slash, no `..` components). Files larger than the max_bytes limit are truncated at the boundary with a marker appended.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"path": {
						"type": "string",
						"description": "Repo-relative path to the file (e.g. apps/marketing/src/components/foo.tsx)"
					}
				},
				"required": ["path"]
			}`),
		},
		{
			Name:        "glob",
			Description: "Find files matching a glob pattern (doublestar-style, so ** matches any depth). Returns a newline-separated list of matching repo-relative paths, sorted lexicographically. Paths that escape the repo root are rejected. Example patterns: `apps/marketing/**/*.tsx`, `**/common.json`, `packages/types/src/*.ts`.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"pattern": {
						"type": "string",
						"description": "Doublestar glob pattern (e.g. apps/**/*.tsx)"
					}
				},
				"required": ["pattern"]
			}`),
		},
		{
			Name:        "grep",
			Description: "Search for a regex pattern across repo files. Returns a newline-separated list of matches in the form `path:line: matched line text`. Optionally scope the search to a list of path prefixes (directories or specific files). If `paths` is omitted the entire repo is searched (minus vendor directories).",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"pattern": {
						"type": "string",
						"description": "Go-flavored regular expression (RE2 syntax)"
					},
					"paths": {
						"type": "array",
						"items": {"type": "string"},
						"description": "Optional repo-relative path prefixes to scope the search. Empty means whole-repo."
					}
				},
				"required": ["pattern"]
			}`),
		},
		{
			Name:        "list_dir",
			Description: "List the contents of a directory. Returns a newline-separated list of entries, each prefixed with 'd/' for directories or 'f/' for files. Useful for exploring the tree when you don't know the exact path to a component.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"path": {
						"type": "string",
						"description": "Repo-relative directory path (use '.' for the repo root)"
					}
				},
				"required": ["path"]
			}`),
		},
	}
}

// Execute dispatches one ToolUse to the matching implementation.
// Returns a ToolResult ready to be appended to the next turn's
// conversation.
//
// "Error" in the ToolResult sense means the TOOL failed (file not
// found, invalid regex, etc.) — something the model can react to.
// Programming errors (bad JSON, unknown tool) also become
// IsError=true tool_results so the model can retry rather than
// tearing down the whole conversation. Only catastrophic failures
// (out-of-memory, unrecoverable I/O) propagate as Go errors.
func (e *ToolExecutor) Execute(use llm.ToolUse) llm.ToolContentBlock {
	block := llm.ToolContentBlock{
		Type:      "tool_result",
		ToolUseID: use.ID,
	}

	out, err := e.dispatch(use)
	if err != nil {
		block.ToolResultContent = "error: " + err.Error()
		block.ToolResultIsError = true
		return block
	}
	block.ToolResultContent = out
	return block
}

// dispatch routes a tool call to the matching executor. Returns
// (output, error) — the caller wraps both in a tool_result block.
func (e *ToolExecutor) dispatch(use llm.ToolUse) (string, error) {
	switch use.Name {
	case "read_file":
		var args struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(use.Input, &args); err != nil {
			return "", fmt.Errorf("read_file: parse input: %w", err)
		}
		return e.readFile(args.Path)

	case "glob":
		var args struct {
			Pattern string `json:"pattern"`
		}
		if err := json.Unmarshal(use.Input, &args); err != nil {
			return "", fmt.Errorf("glob: parse input: %w", err)
		}
		return e.glob(args.Pattern)

	case "grep":
		var args struct {
			Pattern string   `json:"pattern"`
			Paths   []string `json:"paths"`
		}
		if err := json.Unmarshal(use.Input, &args); err != nil {
			return "", fmt.Errorf("grep: parse input: %w", err)
		}
		return e.grep(args.Pattern, args.Paths)

	case "list_dir":
		var args struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(use.Input, &args); err != nil {
			return "", fmt.Errorf("list_dir: parse input: %w", err)
		}
		return e.listDir(args.Path)

	default:
		return "", fmt.Errorf("unknown tool %q", use.Name)
	}
}

// readFile returns a file's contents, size-capped. Empty paths,
// absolute paths, and paths with `..` segments are rejected.
func (e *ToolExecutor) readFile(path string) (string, error) {
	if !isSafeRelPath(path) {
		return "", fmt.Errorf("unsafe or empty path %q", path)
	}
	abs := filepath.Join(e.RepoRoot, path)
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("file not found: %s", path)
	}
	if info.IsDir() {
		return "", fmt.Errorf("path is a directory, use list_dir instead: %s", path)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	if len(data) > e.MaxReadBytes {
		truncated := string(data[:e.MaxReadBytes])
		return truncated + fmt.Sprintf("\n\n[truncated by aidev at %d bytes of %d]\n", e.MaxReadBytes, len(data)), nil
	}
	return string(data), nil
}

// glob walks the repo tree and returns matches against the given
// doublestar pattern. We implement our own doublestar match rather
// than pulling in an external dependency because the matching logic
// is simple: `**` matches any path segment sequence, `*` matches one
// segment without slashes.
func (e *ToolExecutor) glob(pattern string) (string, error) {
	if pattern == "" {
		return "", errors.New("empty pattern")
	}
	// Reject absolute/traversal patterns the same way isSafeRelPath
	// rejects paths. We don't want `glob("/etc/**")` to work.
	if strings.HasPrefix(pattern, "/") || strings.Contains(pattern, "..") {
		return "", fmt.Errorf("unsafe pattern %q", pattern)
	}

	re, err := globToRegexp(pattern)
	if err != nil {
		return "", fmt.Errorf("compile pattern: %w", err)
	}

	var matches []string
	err = filepath.WalkDir(e.RepoRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.IsDir() {
			if shouldSkipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		rel, relErr := filepath.Rel(e.RepoRoot, path)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if re.MatchString(rel) {
			matches = append(matches, rel)
			if len(matches) >= e.MaxGlobHits {
				return errStopWalk
			}
		}
		return nil
	})
	if err != nil && !errors.Is(err, errStopWalk) {
		return "", err
	}

	if len(matches) == 0 {
		return "(no matches)", nil
	}
	sort.Strings(matches)
	result := strings.Join(matches, "\n")
	if len(matches) >= e.MaxGlobHits {
		result += fmt.Sprintf("\n\n[truncated at %d matches — refine your pattern]", e.MaxGlobHits)
	}
	return result, nil
}

// globToRegexp translates a doublestar-style glob into a Go
// regular expression. Rules:
//
//	**       any sequence of path segments (including zero)
//	*        one path segment, no slashes
//	?        one character, no slash
//	[abc]    character class, passed through
//
// Everything else (including /, ., -, _) is escaped.
func globToRegexp(pattern string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	i := 0
	for i < len(pattern) {
		c := pattern[i]
		switch {
		case c == '*' && i+1 < len(pattern) && pattern[i+1] == '*':
			// `**` — consume two, match any path segments.
			// Also eat a following `/` so `a/**/b` matches `a/b`.
			i += 2
			if i < len(pattern) && pattern[i] == '/' {
				i++
			}
			b.WriteString(`(?:.*/)?`)
		case c == '*':
			b.WriteString(`[^/]*`)
			i++
		case c == '?':
			b.WriteString(`[^/]`)
			i++
		case c == '[':
			// Character class, pass through up to the closing `]`.
			end := strings.IndexByte(pattern[i:], ']')
			if end < 0 {
				return nil, fmt.Errorf("unclosed [ in pattern")
			}
			b.WriteString(pattern[i : i+end+1])
			i += end + 1
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
			i++
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

// grep searches the repo (or a subset) for lines matching pattern.
// Uses Go regexp (RE2 syntax). Results are scoped to source files —
// we reuse listSourceFiles's extension filter to avoid grepping
// binaries, node_modules, etc. If paths is non-empty, each path is
// treated as a prefix filter: a match is included only if its
// relative path starts with one of the given prefixes.
func (e *ToolExecutor) grep(pattern string, paths []string) (string, error) {
	if pattern == "" {
		return "", errors.New("empty pattern")
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "", fmt.Errorf("compile pattern: %w", err)
	}

	// Sanity-check scope paths.
	var prefixes []string
	for _, p := range paths {
		p = filepath.ToSlash(strings.TrimSpace(p))
		if p == "" || p == "." {
			continue
		}
		if !isSafeRelPath(p) {
			return "", fmt.Errorf("unsafe scope path %q", p)
		}
		prefixes = append(prefixes, p)
	}

	var hits []string
	err = filepath.WalkDir(e.RepoRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.IsDir() {
			if shouldSkipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !isSourceExt(filepath.Ext(d.Name())) {
			return nil
		}
		rel, relErr := filepath.Rel(e.RepoRoot, path)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if len(prefixes) > 0 && !matchesAnyPrefix(rel, prefixes) {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		for lineNum, line := range strings.Split(string(data), "\n") {
			if re.MatchString(line) {
				// Cap individual line length so one pathological
				// line doesn't explode the output.
				if len(line) > 400 {
					line = line[:400] + "…"
				}
				hits = append(hits, fmt.Sprintf("%s:%d: %s", rel, lineNum+1, line))
				if len(hits) >= e.MaxGrepHits {
					return errStopWalk
				}
			}
		}
		return nil
	})
	if err != nil && !errors.Is(err, errStopWalk) {
		return "", err
	}

	if len(hits) == 0 {
		return "(no matches)", nil
	}
	result := strings.Join(hits, "\n")
	if len(hits) >= e.MaxGrepHits {
		result += fmt.Sprintf("\n\n[truncated at %d matches — narrow the scope with paths=[...]]", e.MaxGrepHits)
	}
	return result, nil
}

// listDir returns a sorted directory listing. Entries are prefixed
// with `d/` or `f/` so the model can distinguish at a glance.
func (e *ToolExecutor) listDir(path string) (string, error) {
	if path == "" {
		path = "."
	}
	if path != "." && !isSafeRelPath(path) {
		return "", fmt.Errorf("unsafe path %q", path)
	}
	abs := e.RepoRoot
	if path != "." {
		abs = filepath.Join(e.RepoRoot, path)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("not found: %s", path)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("not a directory: %s", path)
	}

	entries, err := os.ReadDir(abs)
	if err != nil {
		return "", fmt.Errorf("read dir: %w", err)
	}

	var lines []string
	for _, entry := range entries {
		name := entry.Name()
		if shouldSkipDir(name) && entry.IsDir() {
			continue
		}
		prefix := "f/"
		if entry.IsDir() {
			prefix = "d/"
		}
		lines = append(lines, prefix+name)
		if len(lines) >= e.MaxListHits {
			break
		}
	}
	sort.Strings(lines)
	if len(lines) == 0 {
		return "(empty directory)", nil
	}
	return strings.Join(lines, "\n"), nil
}

// matchesAnyPrefix returns true if rel begins with any of the
// given directory prefixes, or if rel itself is listed as a file
// prefix. Prefixes are matched on path segments: "apps/marketing"
// matches "apps/marketing/foo.tsx" but not "apps/marketing-site/x".
func matchesAnyPrefix(rel string, prefixes []string) bool {
	for _, p := range prefixes {
		if rel == p {
			return true
		}
		if strings.HasPrefix(rel, p+"/") {
			return true
		}
	}
	return false
}

// shouldSkipDir reports whether a directory name is a well-known
// vendor/cache/output directory that should not be searched. Same
// list as listSourceFiles.
func shouldSkipDir(name string) bool {
	switch name {
	case ".git", "node_modules", "vendor", "dist", "build", "target",
		".next", ".venv", "venv", "__pycache__", ".aidev", ".aidev-cache":
		return true
	}
	return false
}

// isSourceExt mirrors the extension allowlist used by
// listSourceFiles — kept in sync by hand rather than shared to
// avoid a cross-file coupling headache.
func isSourceExt(ext string) bool {
	switch strings.ToLower(ext) {
	case ".go", ".py", ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs",
		".rs", ".java", ".kt", ".rb", ".php", ".c", ".cc", ".cpp",
		".h", ".hpp", ".swift", ".m", ".md", ".mdx", ".yaml", ".yml",
		".toml", ".json":
		return true
	}
	return false
}
