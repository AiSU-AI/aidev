package agents

import (
	"context"
	"fmt"
	"strings"

	"github.com/aisu-ai/aidev/internal/llm"
)

// Scout is the first agent in the pipeline. Its job is to read the raw
// repository snapshot and produce a tight, factual brief that downstream
// agents can cite: "this is a Go CLI, its stated purpose is X, its top-level
// layout is Y, it follows principles Z".
//
// Scout is routed to the small (local) tier because its task is extractive,
// not generative. Running it on a 7B local model keeps the hot loop cheap
// and offline-friendly.
type Scout struct {
	Provider llm.Provider
}

// NewScout builds a Scout using the router's mapping for RoleScout.
func NewScout(router *llm.Router) (*Scout, error) {
	p, err := router.For(llm.RoleScout)
	if err != nil {
		return nil, err
	}
	return &Scout{Provider: p}, nil
}

// Run produces a short repo brief. The returned string is also stored on
// the shared Context so the Critic can quote it verbatim.
func (s *Scout) Run(ctx context.Context, c *Context) (string, error) {
	if c == nil || c.Snapshot == nil {
		return "", fmt.Errorf("scout: missing snapshot")
	}
	snap := c.Snapshot

	// Build a compact textual view of the repo for the model. We pass the
	// raw README and agent-md verbatim but truncate at 4 KiB each to stay
	// inside the small tier's context window.
	var b strings.Builder
	fmt.Fprintf(&b, "# Repository at %s\n", snap.Root)
	fmt.Fprintf(&b, "Total files: %d\n", snap.TotalFiles)
	if langs := snap.TopLanguages(8); len(langs) > 0 {
		fmt.Fprintf(&b, "Top languages: %s\n", strings.Join(langs, ", "))
	}
	if len(snap.TopLevelEntries) > 0 {
		fmt.Fprintf(&b, "Top-level entries: %s\n", strings.Join(snap.TopLevelEntries, ", "))
	}
	b.WriteString("\n")

	if snap.ReadmeContent != "" {
		b.WriteString("## README\n\n")
		b.WriteString(truncate(snap.ReadmeContent, 4096))
		b.WriteString("\n\n")
	}
	if snap.AgentMarkdown != "" {
		b.WriteString("## Agent markdown (CLAUDE.md / AGENTS.md)\n\n")
		b.WriteString(truncate(snap.AgentMarkdown, 4096))
		b.WriteString("\n\n")
	}
	if snap.ArchitectureDoc != "" {
		b.WriteString("## ARCHITECTURE.md\n\n")
		b.WriteString(truncate(snap.ArchitectureDoc, 4096))
		b.WriteString("\n\n")
	}
	if snap.CharterContent != "" {
		b.WriteString("## .aidev/charter.md (human-authored product charter)\n\n")
		b.WriteString(truncate(snap.CharterContent, 4096))
		b.WriteString("\n\n")
	}
	if snap.ClarifierContent != "" {
		b.WriteString("## .aidev/clarifier.md (recent clarifier session — human decisions on ambiguous points)\n\n")
		b.WriteString(truncate(snap.ClarifierContent, 4096))
		b.WriteString("\n\n")
	}

	// Directory tree goes LAST, framed as filesystem ground truth.
	// Small-tier models show recency bias; putting the tree after the
	// prose makes it the most-attended section. The frame is deliberately
	// blunt ("OVERRIDES docs") because an earlier revision placed the
	// tree before the docs and the model still pulled directory names
	// from stale README prose instead of the actual filesystem.
	if snap.DirectoryTree != "" {
		b.WriteString("## FILESYSTEM GROUND TRUTH (depth-2 directory tree — overrides any architecture description in the docs above)\n\n")
		b.WriteString("This is what the filesystem actually contains RIGHT NOW. If the README or CLAUDE.md describes a directory layout that does not match this tree, the docs are stale. Name directories as they appear here, not as they appear in prose.\n\n")
		b.WriteString("```\n")
		b.WriteString(snap.DirectoryTree)
		b.WriteString("```\n\n")
	}

	system := `You are the Scout for aidev, a multi-agent coding tool.
Your job is to produce a FACTUAL, concise brief of the repository a developer
is asking you to work on. Do NOT propose changes. Do NOT speculate.

Produce exactly these sections in Markdown:

## Purpose
One paragraph: what this repository is for, as stated by its own docs.
If the docs disagree, call that out.

## Architecture
3-6 bullets on how the code is organised. RULE: every directory name
you mention MUST appear verbatim in the "FILESYSTEM GROUND TRUTH"
section of the user message. Do not invent directories from similar
projects. Do not copy directory names from the README or CLAUDE.md —
those docs may be stale, the filesystem is not. If the tree shows
"apps/web-spa" and the docs say "apps/web", you MUST write
"apps/web-spa" and note the drift in Unknowns below.

## Stated principles
Bullets drawn verbatim from any agent markdown or architecture doc.

## Unknowns
Bullets listing questions the Critic will need answered before evaluating
any new feature. If any directory or module named in the README or
CLAUDE.md does NOT appear in the filesystem ground truth (or vice
versa), list that drift here explicitly — the docs have become
unreliable on that point and the Critic needs to know.`

	req := llm.Request{
		System: system,
		Messages: []llm.Message{
			{Role: "user", Content: b.String()},
		},
	}

	resp, err := s.Provider.Complete(ctx, req)
	if err != nil {
		return "", fmt.Errorf("scout: %w", err)
	}
	c.ScoutReport = resp.Content
	return resp.Content, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n\n...[truncated]..."
}
