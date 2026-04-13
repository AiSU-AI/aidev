package github

import (
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// ResolveIssueRef turns a user-provided issue reference into a
// canonical GitHub URL. The reference is either:
//
//   - A full GitHub issue URL: https://github.com/owner/repo/issues/N
//     → returned verbatim (after validation).
//
//   - A bare integer: 533
//     → resolved against the git remote origin URL of repoRoot. The
//     function shells out to `git -C repoRoot config --get
//     remote.origin.url`, parses the remote, and constructs the
//     canonical URL.
//
// The second form is the ergonomic win: inside a cloned repo, users
// can type just `533` and aidev figures out the owner/repo from
// `.git/config` — no retyping the same URL prefix every invocation.
//
// Returns (canonical URL, owner, repo, number, error). On any parse
// failure, the error contains enough context to point the user at the
// exact problem (bad URL shape, missing git remote, unrecognised
// remote format).
func ResolveIssueRef(ref, repoRoot string) (url, owner, repo string, number int, err error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", "", "", 0, errors.New("github: empty issue reference")
	}

	// Full URL form.
	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		owner, repo, number, err = ParseURL(ref)
		if err != nil {
			return "", "", "", 0, err
		}
		return ref, owner, repo, number, nil
	}

	// Bare-number form. repoRoot is required.
	n, err := strconv.Atoi(ref)
	if err != nil {
		return "", "", "", 0, fmt.Errorf("github: %q is neither a URL nor a number", ref)
	}
	if n <= 0 {
		return "", "", "", 0, fmt.Errorf("github: issue number must be positive, got %d", n)
	}
	if repoRoot == "" {
		return "", "", "", 0, errors.New("github: cannot resolve bare issue number without a repo path")
	}

	owner, repo, err = InferOwnerRepoFromGit(repoRoot)
	if err != nil {
		return "", "", "", 0, fmt.Errorf("github: resolve %q via git remote: %w", ref, err)
	}
	return fmt.Sprintf("https://github.com/%s/%s/issues/%d", owner, repo, n), owner, repo, n, nil
}

// InferOwnerRepoFromGit reads the `remote.origin.url` config from the
// git repository rooted at repoRoot and parses it into owner and repo
// components. Supports the three canonical remote URL formats:
//
//   - https://github.com/owner/repo(.git)
//   - git@github.com:owner/repo(.git)
//   - ssh://git@github.com/owner/repo(.git)
//
// Other hosts (gitlab, bitbucket, self-hosted) currently return an
// "unsupported remote" error because aidev's issue URL template is
// GitHub-specific. Extending to other forges is straightforward when
// we need it.
func InferOwnerRepoFromGit(repoRoot string) (owner, repo string, err error) {
	cmd := exec.Command("git", "-C", repoRoot, "config", "--get", "remote.origin.url")
	out, cmdErr := cmd.Output()
	if cmdErr != nil {
		return "", "", fmt.Errorf("git config remote.origin.url in %s: %w", repoRoot, cmdErr)
	}
	return ParseRemoteURL(strings.TrimSpace(string(out)))
}

// ParseRemoteURL parses a GitHub remote URL into owner + repo.
// Handles the three canonical formats git emits from `git clone`:
//
//	https://github.com/owner/repo          (+ optional .git suffix)
//	git@github.com:owner/repo              (+ optional .git suffix)
//	ssh://git@github.com/owner/repo        (+ optional .git suffix)
//
// Returns an error for any non-github.com remote so users get a clear
// "this is only implemented for GitHub" message rather than silent
// wrong behaviour.
func ParseRemoteURL(url string) (owner, repo string, err error) {
	url = strings.TrimSuffix(strings.TrimSpace(url), ".git")
	if url == "" {
		return "", "", errors.New("empty remote url")
	}

	var tail string
	switch {
	case strings.HasPrefix(url, "https://github.com/"):
		tail = strings.TrimPrefix(url, "https://github.com/")
	case strings.HasPrefix(url, "http://github.com/"):
		tail = strings.TrimPrefix(url, "http://github.com/")
	case strings.HasPrefix(url, "git@github.com:"):
		tail = strings.TrimPrefix(url, "git@github.com:")
	case strings.HasPrefix(url, "ssh://git@github.com/"):
		tail = strings.TrimPrefix(url, "ssh://git@github.com/")
	default:
		return "", "", fmt.Errorf("unsupported remote url: %q (only github.com is implemented)", url)
	}

	parts := strings.SplitN(tail, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("malformed github remote url: %q", url)
	}
	// Trim any trailing slash or extra path fragments (shouldn't
	// happen for well-formed remotes but defensive).
	return parts[0], strings.SplitN(parts[1], "/", 2)[0], nil
}
