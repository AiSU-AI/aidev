// Package runpath resolves the per-issue artifact directory aidev writes
// generated outputs into. The goal is to keep the user's target repo tidy:
// only human-authored artifacts (charter.md, principles.yaml,
// sketch-rubric.yaml) live under <repo>/.aidev/. Everything aidev
// generates each run (clarifier.md, architect-output.md, followups.md,
// proposed.patch) lives under $XDG_DATA_HOME/aidev/runs/<id>/ instead,
// out of sight of `git status`.
package runpath

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// RunDir returns the per-run artifact directory for the given issue,
// creating it (recursively, 0o755) if it does not yet exist.
//
// The path layout is:
//
//	$XDG_DATA_HOME/aidev/runs/<owner>-<repo>-<issue>/
//
// When XDG_DATA_HOME is unset, the macOS/Linux default of
// $HOME/.local/share is used. The owner+repo+issue tuple uniquely
// identifies the issue across all GitHub installations the user
// interacts with.
//
// Subsequent runs against the SAME issue overwrite their predecessor's
// files inside the directory. This matches the existing per-artifact
// "overwrite each run" behaviour (see e.g. reviewer.Persist) and keeps
// the per-issue archive small and predictable. A future cleanup
// subcommand can prune the runs/ tree by mtime.
func RunDir(owner, repo string, issue int) (string, error) {
	if strings.TrimSpace(owner) == "" {
		return "", errors.New("runpath: empty owner")
	}
	if strings.TrimSpace(repo) == "" {
		return "", errors.New("runpath: empty repo")
	}
	if issue <= 0 {
		return "", fmt.Errorf("runpath: invalid issue number %d", issue)
	}

	root, err := DataRoot()
	if err != nil {
		return "", err
	}

	dir := filepath.Join(root, "runs", slugFor(owner, repo, issue))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("runpath: mkdir %s: %w", dir, err)
	}
	return dir, nil
}

// DataRoot returns the base aidev data directory (without the runs/
// subdirectory), creating its parent on demand. Useful for tests and for
// callers that want a sibling directory (e.g. cache, logs).
//
// Resolution order:
//  1. $XDG_DATA_HOME/aidev
//  2. $HOME/.local/share/aidev
//
// Returns an error only when both XDG_DATA_HOME and HOME are unset,
// which would mean the process is running in a degenerate environment
// where no per-user data location is discoverable.
func DataRoot() (string, error) {
	if v := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); v != "" {
		return filepath.Join(v, "aidev"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", errors.New("runpath: cannot resolve $XDG_DATA_HOME or $HOME")
	}
	return filepath.Join(home, ".local", "share", "aidev"), nil
}

// slugFor produces the per-issue subdirectory name. The format is
// <owner>-<repo>-<issue>, with any character outside [a-zA-Z0-9._-]
// replaced by '_' so a forward slash or shell metacharacter from a
// hostile owner/repo name can't escape the directory.
func slugFor(owner, repo string, issue int) string {
	safe := func(s string) string {
		return slugSafeRe.ReplaceAllString(s, "_")
	}
	return fmt.Sprintf("%s-%s-%d", safe(owner), safe(repo), issue)
}

var slugSafeRe = regexp.MustCompile(`[^a-zA-Z0-9._-]`)
