package agents

import (
	"context"
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

// Run asks the LLM to produce a unified diff that implements the chosen
// Sketch. The Implementer is given the full context chain: issue, scout
// brief, critic report, chosen sketch, and the names of the top-level
// source files in the repo (to anchor the diff at real paths). It does
// NOT send file contents — v0.3a ships without file-content loading, so
// the resulting patch is a first-draft the user will almost certainly
// edit. A better context-loading story is tracked for v0.3a.1.
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

	// Assemble the user message. We include the full context chain plus
	// a small file listing (names only, no contents) so the model knows
	// what paths actually exist. Contents are deliberately omitted in
	// v0.3a — loading them correctly is its own design problem.
	var user strings.Builder
	fmt.Fprintf(&user, "## Issue %s/%s#%d: %s\n\n", c.Issue.Owner, c.Issue.Repo, c.Issue.Number, c.Issue.Title)
	user.WriteString(c.Issue.Body)
	user.WriteString("\n\n## Scout brief\n\n")
	user.WriteString(c.ScoutReport)
	user.WriteString("\n\n## Critic report\n\n")
	user.WriteString(c.CriticReport)
	fmt.Fprintf(&user, "\n\n## Chosen sketch %d: %s\n\n", chosen.Number, chosen.Title)
	user.WriteString(chosen.Markdown)

	files := listSourceFiles(c.Snapshot.Root, 150)
	if len(files) > 0 {
		user.WriteString("\n\n## File inventory (paths only; contents not included in v0.3a)\n\n")
		for _, f := range files {
			user.WriteString("- ")
			user.WriteString(f)
			user.WriteString("\n")
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
