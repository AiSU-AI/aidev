package repo

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ResolveLocalPath turns a raw `-repo` flag value into an absolute
// local path. The happy path is just `filepath.Abs`.
//
// When the resolved path does not exist (or exists but is not a
// directory), and the raw input looks like a remote reference —
// `https://…`, `git@host:…`, or the bare `owner/repo` shorthand
// matching the `gh` CLI convention — the returned error is enriched
// with an actionable suggestion pointing the user at `gh repo clone`.
//
// Validation order is `resolve → stat → enrich-on-missing` (Sketch 3
// from aidev #45). This is deliberate: a legitimate local directory
// that happens to match the `owner/repo` shorthand (e.g. the user
// maintains `foo/bar` as a literal relative path in their workspace)
// is accepted, because `os.Stat` succeeds before the URL-shape check
// runs. The URL-shape heuristics are a diagnostic aid attached to an
// existing failure path, not a new gate.
//
// Issue #45 (AiSU-AI/aidev).
func ResolveLocalPath(input string) (string, error) {
	abs, err := filepath.Abs(input)
	if err != nil {
		return "", fmt.Errorf("resolve repo path %q: %w", input, err)
	}
	info, statErr := os.Stat(abs)
	if statErr == nil {
		if !info.IsDir() {
			return "", fmt.Errorf("repo path %q exists but is not a directory", abs)
		}
		return abs, nil
	}
	// Stat failed. Enrich the error if the raw input looks remote.
	if owner, repoName, ok := detectURLShape(input); ok {
		return "", fmt.Errorf(
			"-repo expects a local filesystem path, not a remote reference.\n"+
				"    Detected a URL-shaped input: %s (parsed as %s/%s).\n"+
				"    Run 'gh repo clone %s/%s' first, then pass the local clone path.\n"+
				"    (Auto-cloning from URLs is tracked as a stretch goal — see issue #45.)",
			input, owner, repoName, owner, repoName,
		)
	}
	return "", fmt.Errorf("repo path %q does not exist: %w", abs, statErr)
}

// detectURLShape classifies a raw `-repo` input as a remote reference
// and, if so, extracts the owner and repo for the user-facing error
// message. Returns (owner, repo, true) on a match.
//
// Three shapes are recognised:
//
//  1. HTTP(S) clone URLs: `https://host/owner/repo(.git)?`
//  2. SSH clone URLs:     `git@host:owner/repo(.git)?`
//  3. `gh` shorthand:     `owner/repo`
//
// The shorthand is intentionally the last-checked and strictest: it
// only matches when the string contains exactly one `/`, no URL scheme,
// no colon, and conforms to the narrow `[\w.-]+/[\w.-]+` shape that
// GitHub's own slug rules permit. That rules out ambiguous local
// paths like `docs/readme` (contains no dot-free owner) but still
// catches genuine shorthand like `AiSU-AI/aidev`.
func detectURLShape(input string) (owner, repo string, ok bool) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return "", "", false
	}

	// HTTP(S): https://host/owner/repo(.git)?
	if m := httpCloneRe.FindStringSubmatch(trimmed); m != nil {
		return m[1], stripGitSuffix(m[2]), true
	}

	// SSH: git@host:owner/repo(.git)?
	if m := sshCloneRe.FindStringSubmatch(trimmed); m != nil {
		return m[1], stripGitSuffix(m[2]), true
	}

	// Bare shorthand: owner/repo
	if m := shorthandRe.FindStringSubmatch(trimmed); m != nil {
		return m[1], stripGitSuffix(m[2]), true
	}

	return "", "", false
}

func stripGitSuffix(s string) string {
	return strings.TrimSuffix(s, ".git")
}

// httpCloneRe matches HTTPS/HTTP clone URLs. Host portion is
// non-empty; path is exactly two segments (owner/repo), optionally
// followed by `.git` and optionally a trailing slash or query.
var httpCloneRe = regexp.MustCompile(`^https?://[^/\s]+/([\w.-]+)/([\w.-]+?)(?:\.git)?/?$`)

// sshCloneRe matches SSH clone URLs in the `git@host:path` form.
// Host portion is non-empty; path is `owner/repo` with optional
// `.git`.
var sshCloneRe = regexp.MustCompile(`^git@[^:\s]+:([\w.-]+)/([\w.-]+?)(?:\.git)?/?$`)

// shorthandRe matches the bare `owner/repo` form. Strict on purpose:
// no URL scheme, no colon, exactly one slash, alphanumerics +
// `._-` in each segment, and neither segment may start with `.` so
// local relative paths like `./foo` and `../bar` fall through to the
// normal filesystem error instead of triggering URL-shape enrichment.
// GitHub usernames and repository names cannot start with `.` anyway.
var shorthandRe = regexp.MustCompile(`^([\w-][\w.-]*)/([\w-][\w.-]*?)(?:\.git)?$`)
