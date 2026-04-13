package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/aisu-ai/aidev/internal/llm"
)

// Implementer is the v0.3a agent that takes a chosen Architect Sketch
// and produces a unified git diff (patch) that applies it. It writes
// nothing to the target repo — the output is a patch the user reviews,
// then applies with `git apply` themselves. This keeps aidev on the
// "propose, never mutate" side of the safety line until we have better
// guardrails (sandboxing, test gating) for automatic application.
type Implementer struct {
	Provider llm.Provider
}

// NewImplementer builds an Implementer from the router's RoleImplementer
// mapping (defaults to the medium tier in the shipped config — code
// generation is a high-volume task that doesn't need frontier reasoning
// on every token).
func NewImplementer(router *llm.Router) (*Implementer, error) {
	p, err := router.For(llm.RoleImplementer)
	if err != nil {
		return nil, err
	}
	return &Implementer{Provider: p}, nil
}

// Patch is the output of an Implementer run.
type Patch struct {
	// Diff is the raw unified diff, ready to hand to `git apply`.
	Diff string

	// Path is the location where the diff was written on disk, relative
	// or absolute depending on what the caller passed in. Empty if the
	// caller did not request a write.
	Path string

	// FilesTouched is the list of file paths that appear as targets in
	// the unified diff's `+++ b/<path>` lines. Parsed from Diff for
	// display purposes; callers shouldn't trust it for security
	// decisions (always re-derive from the diff itself).
	FilesTouched []string
}

// Size and count limits for the file-content loading pass. These keep
// the second-turn prompt bounded regardless of what the first-turn
// file picker returns.
const (
	maxRequestedFiles = 15
	maxFileBytes      = 10 * 1024 // 10 KiB per file
)

// Run asks the LLM to produce a unified diff that implements the chosen
// Sketch. v0.3a.1 drives a **two-turn conversation**:
//
//	Turn 1: send the full context chain plus a file inventory and ask
//	        the model to list (as a JSON array) the files it needs to
//	        see the contents of to write a correct patch.
//	Turn 2: read those files from disk, embed their contents in the
//	        prompt, and ask for the unified diff.
//
// This dramatically improves diff quality compared to v0.3a, which only
// sent file names and not contents — the resulting diffs often failed
// to apply cleanly because the model guessed at context lines.
//
// chosen must be non-nil and must be one of the sketches already
// produced by the Architect (live in c.Sketches). The caller is
// responsible for the selection; this function doesn't validate that
// `chosen` came from the same orchestrator run.
func (i *Implementer) Run(ctx context.Context, c *Context, chosen *Sketch) (*Patch, error) {
	if c == nil || c.Issue == nil {
		return nil, errors.New("implementer: missing issue")
	}
	if chosen == nil {
		return nil, errors.New("implementer: no sketch chosen")
	}
	if c.Snapshot == nil {
		return nil, errors.New("implementer: no repo snapshot")
	}

	// Turn 1: ask the model which files it needs to see.
	wanted, err := i.selectFiles(ctx, c, chosen)
	if err != nil {
		return nil, fmt.Errorf("implementer: file selection: %w", err)
	}

	// Load the requested file contents (size-capped, path-validated).
	// File read errors are logged into the contents map rather than
	// aborting — the model can still work with partial information.
	contents := readFiles(c.Snapshot.Root, wanted)

	// Turn 2: produce the diff with real file contents in hand.
	return i.generateDiff(ctx, c, chosen, contents)
}

// fileSelectionJSONRe extracts a JSON array from a potentially messy
// LLM response. It matches the first `[...]` block in the response,
// which handles both bare JSON and JSON wrapped in a markdown code
// fence or surrounded by explanatory prose.
var fileSelectionJSONRe = regexp.MustCompile(`(?s)\[[^\]]*\]`)

// selectFiles is turn 1. It sends the full context chain plus the file
// inventory and asks the LLM to return a JSON array of up to
// maxRequestedFiles relative paths that it needs to see the contents
// of before writing the patch.
func (i *Implementer) selectFiles(ctx context.Context, c *Context, chosen *Sketch) ([]string, error) {
	system := "You are the Implementer's file picker for aidev, a multi-agent coding tool.\n" +
		"\n" +
		"Given the context below, return ONLY a JSON array of relative file\n" +
		"paths (up to " + fmt.Sprint(maxRequestedFiles) + ") that you need to see\n" +
		"the contents of before you can write a correct unified diff for the\n" +
		"chosen sketch. Include:\n" +
		"\n" +
		"  - files you plan to modify\n" +
		"  - files whose current behaviour you need to understand in order\n" +
		"    to modify another file correctly\n" +
		"  - test files adjacent to the code you plan to touch\n" +
		"\n" +
		"DO NOT include files you don't actually need. Fewer, more-relevant\n" +
		"files is better than a long list. The second turn's prompt grows with\n" +
		"each file you request.\n" +
		"\n" +
		"Output format: EXACTLY a JSON array of strings, no code fence, no\n" +
		"surrounding prose. Example:\n" +
		"\n" +
		"[\"internal/foo/foo.go\", \"internal/foo/foo_test.go\", \"cmd/app/main.go\"]\n" +
		"\n" +
		"Paths MUST be relative to the repository root (no leading slash, no\n" +
		"'..' components). Paths to files that don't currently exist in the\n" +
		"repository (new files you plan to create) MUST NOT appear here — we\n" +
		"only fetch existing content in this turn."

	var user strings.Builder
	fmt.Fprintf(&user, "## Issue %s/%s#%d: %s\n\n", c.Issue.Owner, c.Issue.Repo, c.Issue.Number, c.Issue.Title)
	user.WriteString(c.Issue.Body)
	user.WriteString("\n\n## Scout brief\n\n")
	user.WriteString(c.ScoutReport)
	user.WriteString("\n\n## Critic report\n\n")
	user.WriteString(c.CriticReport)
	fmt.Fprintf(&user, "\n\n## Chosen sketch %d: %s\n\n", chosen.Number, chosen.Title)
	user.WriteString(chosen.Markdown)

	files := listSourceFiles(c.Snapshot.Root, 200)
	if len(files) > 0 {
		user.WriteString("\n\n## File inventory (paths only, pick from this list)\n\n")
		for _, f := range files {
			user.WriteString("- ")
			user.WriteString(f)
			user.WriteString("\n")
		}
	}

	resp, err := i.Provider.Complete(ctx, llm.Request{
		System: system,
		Messages: []llm.Message{
			{Role: "user", Content: user.String()},
		},
	})
	if err != nil {
		return nil, err
	}

	return parseFileSelection(resp.Content)
}

// parseFileSelection extracts a []string from the model's response to
// the file-picker prompt. It tolerates:
//
//   - bare JSON arrays
//   - JSON wrapped in a markdown code fence
//   - JSON preceded or followed by prose
//
// Exported for tests. Duplicate and clearly invalid entries (absolute
// paths, paths with '..' components, paths longer than 1 KB) are
// silently dropped. The resulting slice is capped at maxRequestedFiles.
func parseFileSelection(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("empty file selection response")
	}
	// Fast path: the whole response is already a JSON array.
	var arr []string
	if err := json.Unmarshal([]byte(raw), &arr); err == nil {
		return cleanFileList(arr), nil
	}
	// Fallback: pull the first [...] block out of the response.
	match := fileSelectionJSONRe.FindString(raw)
	if match == "" {
		return nil, fmt.Errorf("no JSON array in file selection response: %q", firstLine(raw))
	}
	if err := json.Unmarshal([]byte(match), &arr); err != nil {
		return nil, fmt.Errorf("parse JSON array: %w", err)
	}
	return cleanFileList(arr), nil
}

// cleanFileList strips invalid paths (absolute, escape attempts, too
// long), de-duplicates by value preserving first-seen order, and caps
// the list length at maxRequestedFiles.
func cleanFileList(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, p := range in {
		p = strings.TrimSpace(p)
		if !isSafeRelPath(p) {
			continue
		}
		if seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
		if len(out) >= maxRequestedFiles {
			break
		}
	}
	return out
}

// isSafeRelPath rejects paths that:
//
//   - are empty
//   - are absolute
//   - contain any '..' component (even if filepath.Clean would resolve
//     the traversal to something safe — we treat any traversal-looking
//     input as suspicious by default)
//   - exceed 1 KB (nothing legitimate is that long)
//
// Uses raw path splitting rather than filepath.Clean because we want to
// reject the ORIGINAL string shape, not its resolved form.
func isSafeRelPath(p string) bool {
	if p == "" {
		return false
	}
	if len(p) > 1024 {
		return false
	}
	if filepath.IsAbs(p) {
		return false
	}
	// Split on both '/' and the OS separator so Windows-style paths
	// are rejected the same way Unix-style paths are.
	for _, part := range strings.FieldsFunc(p, func(r rune) bool {
		return r == '/' || r == filepath.Separator
	}) {
		if part == ".." {
			return false
		}
	}
	return true
}

// readFiles loads the file contents for the given paths, relative to
// repoRoot. Missing files are skipped silently (they may have been
// requested by the model but don't exist on disk). Files larger than
// maxFileBytes are truncated with a marker at the end.
//
// The returned map is keyed by the ORIGINAL requested path (not the
// resolved absolute path), so the second-turn prompt can cite the same
// strings the first turn returned.
func readFiles(repoRoot string, paths []string) map[string]string {
	out := make(map[string]string, len(paths))
	for _, p := range paths {
		if !isSafeRelPath(p) {
			continue
		}
		abs := filepath.Join(repoRoot, p)
		info, err := os.Stat(abs)
		if err != nil || info.IsDir() {
			continue
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			continue
		}
		if len(data) > maxFileBytes {
			data = append(data[:maxFileBytes], []byte("\n// ...[truncated by aidev at "+fmt.Sprint(maxFileBytes)+" bytes]...")...)
		}
		out[p] = string(data)
	}
	return out
}

// generateDiff is turn 2. Same prompt shape as the old single-turn
// Run() but with the requested file contents embedded. Kept separate
// from selectFiles so tests and callers can exercise the two phases
// independently.
func (i *Implementer) generateDiff(ctx context.Context, c *Context, chosen *Sketch, contents map[string]string) (*Patch, error) {

	system := "You are the Implementer for aidev, a multi-agent coding tool.\n" +
		"\n" +
		"The developer has chosen one of the Architect's sketches. Your job is to\n" +
		"produce a UNIFIED GIT DIFF that implements the sketch against the target\n" +
		"repository.\n" +
		"\n" +
		"REQUIREMENTS:\n" +
		"\n" +
		"1. Output MUST be a single unified diff in the standard git format:\n" +
		"\n" +
		"       diff --git a/path/to/file b/path/to/file\n" +
		"       --- a/path/to/file\n" +
		"       +++ b/path/to/file\n" +
		"       @@ -lineno,count +lineno,count @@\n" +
		"       -old line\n" +
		"       +new line\n" +
		"\n" +
		"   For new files, use /dev/null as the 'a' side.\n" +
		"   For deletions, use /dev/null as the 'b' side.\n" +
		"\n" +
		"2. Use the EXACT file paths the target repository already uses. Do not\n" +
		"   invent directories. Do not prefix with './' — use bare paths.\n" +
		"\n" +
		"3. Keep the diff MINIMAL. Only include files the chosen sketch actually\n" +
		"   needs to touch. Do not reformat unrelated code. Follow the Boy Scout\n" +
		"   Rule but stay inside the boundary of the change.\n" +
		"\n" +
		"4. Include docstrings / comments where the sketch implies new public\n" +
		"   API. Follow the repository's existing conventions (go doc comments,\n" +
		"   python docstrings, etc. as appropriate).\n" +
		"\n" +
		"5. Include tests for every meaningful behaviour the diff adds. Tests\n" +
		"   belong in the same diff, not a follow-up.\n" +
		"\n" +
		"6. Do NOT wrap the diff in a Markdown code fence. Do NOT prefix the diff\n" +
		"   with commentary. The first line of your response MUST be either\n" +
		"   'diff --git' (a change) or '# no-op' (if you've decided the sketch\n" +
		"   doesn't require any code changes — e.g. it was a documentation-only\n" +
		"   sketch).\n" +
		"\n" +
		"7. If you are unsure about a file's current content and need more\n" +
		"   context, do your best with what you know and annotate uncertain\n" +
		"   regions with '# TODO(aidev): verify ...' inside the diff content.\n" +
		"\n" +
		"Stay realistic. The user will apply this diff with 'git apply' and\n" +
		"review it; a partially-correct starting point is more useful than an\n" +
		"attempt at a whole-repo rewrite."

	// Assemble the user message. v0.3a.1 includes the CONTENTS of the
	// files the first turn's picker asked for, so the model can anchor
	// its diff at exact context lines.
	var user strings.Builder
	fmt.Fprintf(&user, "## Issue %s/%s#%d: %s\n\n", c.Issue.Owner, c.Issue.Repo, c.Issue.Number, c.Issue.Title)
	user.WriteString(c.Issue.Body)
	user.WriteString("\n\n## Scout brief\n\n")
	user.WriteString(c.ScoutReport)
	user.WriteString("\n\n## Critic report\n\n")
	user.WriteString(c.CriticReport)
	fmt.Fprintf(&user, "\n\n## Chosen sketch %d: %s\n\n", chosen.Number, chosen.Title)
	user.WriteString(chosen.Markdown)

	// Embed the requested file contents verbatim. We use a stable sort
	// order by path so the prompt is deterministic across runs.
	if len(contents) > 0 {
		paths := make([]string, 0, len(contents))
		for p := range contents {
			paths = append(paths, p)
		}
		sortStrings(paths)

		user.WriteString("\n\n## Current file contents (for exact-line anchoring)\n\n")
		for _, p := range paths {
			fmt.Fprintf(&user, "### %s\n\n```\n%s\n```\n\n", p, contents[p])
		}
		user.WriteString("Only the files above are shown. If you need others to write a correct diff, annotate TODOs in your response — the user can re-run after adding them to the Scout's file inventory.\n")
	} else {
		// Fallback to the v0.3a behaviour (paths only) when the picker
		// returned nothing useful. Degrades gracefully.
		files := listSourceFiles(c.Snapshot.Root, 150)
		if len(files) > 0 {
			user.WriteString("\n\n## File inventory (paths only — the file picker did not identify any files to load)\n\n")
			for _, f := range files {
				user.WriteString("- ")
				user.WriteString(f)
				user.WriteString("\n")
			}
		}
	}

	user.WriteString("\n\n## Principles\n\n")
	for _, p := range c.Principles {
		fmt.Fprintf(&user, "- **%s** — %s\n", p.Name, p.Summary)
	}

	resp, err := i.Provider.Complete(ctx, llm.Request{
		System: system,
		Messages: []llm.Message{
			{Role: "user", Content: user.String()},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("implementer: %w", err)
	}

	diff := strings.TrimSpace(resp.Content)
	if diff == "" {
		return nil, errors.New("implementer: empty response")
	}
	// Strip a leading markdown code fence if the model ignored the
	// "no code fence" instruction — we've seen it happen in practice.
	diff = stripCodeFence(diff)

	if !strings.HasPrefix(diff, "diff --git") && !strings.HasPrefix(diff, "# no-op") {
		return nil, fmt.Errorf("implementer: expected 'diff --git' or '# no-op', got %q", firstLine(diff))
	}

	return &Patch{
		Diff:         diff,
		FilesTouched: parseFilesFromDiff(diff),
	}, nil
}

// WriteTo persists a Patch to a file (default: `<repo>/.aidev/proposed.patch`).
// Returns the absolute path written. Overwrites any existing file at that
// path — we keep only one proposed patch per repo at a time.
func (p *Patch) WriteTo(repoRoot string) (string, error) {
	if p == nil {
		return "", errors.New("implementer: nil patch")
	}
	dir := filepath.Join(repoRoot, ".aidev")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("implementer: mkdir: %w", err)
	}
	path := filepath.Join(dir, "proposed.patch")
	body := p.Diff + "\n\n# Written by aidev at " + time.Now().UTC().Format(time.RFC3339) + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return "", fmt.Errorf("implementer: write: %w", err)
	}
	p.Path = path
	return path, nil
}

// diffFileRe matches the "+++ b/<path>" target line in a unified diff.
// We strip leading "+++ " and the "b/" prefix before recording.
var diffFileRe = regexp.MustCompile(`(?m)^\+\+\+ b/(.+)$`)

// parseFilesFromDiff extracts the list of target files from a unified
// diff's `+++ b/<path>` lines. Exported for tests — but note that
// callers should never use this for security decisions; the diff
// itself is authoritative.
func parseFilesFromDiff(diff string) []string {
	matches := diffFileRe.FindAllStringSubmatch(diff, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		// Skip /dev/null targets (these are file deletions).
		if m[1] == "/dev/null" {
			continue
		}
		out = append(out, strings.TrimSpace(m[1]))
	}
	return out
}

// stripCodeFence removes a leading and trailing ``` fence if present.
// The Implementer prompt says not to wrap, but models sometimes do
// anyway.
func stripCodeFence(s string) string {
	lines := strings.Split(s, "\n")
	if len(lines) == 0 {
		return s
	}
	if strings.HasPrefix(lines[0], "```") {
		lines = lines[1:]
	}
	if n := len(lines); n > 0 && strings.HasPrefix(lines[n-1], "```") {
		lines = lines[:n-1]
	}
	return strings.Join(lines, "\n")
}

// listSourceFiles returns up to `limit` source file paths from rootDir,
// filtered to common source extensions and skipping the usual vendor
// directories. The list is relative to rootDir so the model sees the
// same paths git does.
func listSourceFiles(rootDir string, limit int) []string {
	var out []string
	skipDirs := map[string]bool{
		".git": true, "node_modules": true, "vendor": true, "dist": true,
		"build": true, "target": true, ".next": true, ".venv": true, "venv": true,
		"__pycache__": true, ".aidev": true, ".aidev-cache": true,
	}
	sourceExts := map[string]bool{
		".go": true, ".py": true, ".ts": true, ".tsx": true, ".js": true, ".jsx": true,
		".rs": true, ".java": true, ".kt": true, ".rb": true, ".php": true, ".c": true,
		".cc": true, ".cpp": true, ".h": true, ".hpp": true, ".swift": true, ".m": true,
		".md": true, ".yaml": true, ".yml": true, ".toml": true, ".json": true,
	}

	_ = filepath.Walk(rootDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil {
			return nil
		}
		if info.IsDir() {
			if skipDirs[info.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !sourceExts[strings.ToLower(filepath.Ext(info.Name()))] {
			return nil
		}
		rel, err := filepath.Rel(rootDir, path)
		if err != nil {
			return nil
		}
		out = append(out, rel)
		if len(out) >= limit {
			return errStopWalk
		}
		return nil
	})
	return out
}

// errStopWalk is a sentinel used to short-circuit filepath.Walk once we
// have enough file paths. WalkFunc returning a non-nil error that isn't
// filepath.SkipDir aborts the walk.
var errStopWalk = errors.New("stop walk")

// sortStrings is a tiny in-place insertion sort used by generateDiff to
// stabilise the file-contents section ordering. We avoid importing
// "sort" here because this file already has a tight import list and
// this is the only call site.
func sortStrings(a []string) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j-1] > a[j]; j-- {
			a[j-1], a[j] = a[j], a[j-1]
		}
	}
}
