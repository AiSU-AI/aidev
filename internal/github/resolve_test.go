package github

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestParseRemoteURLHTTPS(t *testing.T) {
	cases := []struct {
		in            string
		wantOwner     string
		wantRepo      string
	}{
		{"https://github.com/aisu-ai/aidev.git", "aisu-ai", "aidev"},
		{"https://github.com/aisu-ai/aidev", "aisu-ai", "aidev"},
		{"https://github.com/AiSU-AI/Company-Site.git", "AiSU-AI", "Company-Site"},
		{"http://github.com/foo/bar", "foo", "bar"},
	}
	for _, c := range cases {
		o, r, err := ParseRemoteURL(c.in)
		if err != nil {
			t.Errorf("ParseRemoteURL(%q) err = %v", c.in, err)
			continue
		}
		if o != c.wantOwner || r != c.wantRepo {
			t.Errorf("ParseRemoteURL(%q) = %q, %q; want %q, %q", c.in, o, r, c.wantOwner, c.wantRepo)
		}
	}
}

func TestParseRemoteURLSSH(t *testing.T) {
	cases := []struct {
		in        string
		wantOwner string
		wantRepo  string
	}{
		{"git@github.com:aisu-ai/aidev.git", "aisu-ai", "aidev"},
		{"git@github.com:aisu-ai/aidev", "aisu-ai", "aidev"},
		{"ssh://git@github.com/aisu-ai/aidev.git", "aisu-ai", "aidev"},
		{"ssh://git@github.com/AiSU-AI/Company-Site", "AiSU-AI", "Company-Site"},
	}
	for _, c := range cases {
		o, r, err := ParseRemoteURL(c.in)
		if err != nil {
			t.Errorf("ParseRemoteURL(%q) err = %v", c.in, err)
			continue
		}
		if o != c.wantOwner || r != c.wantRepo {
			t.Errorf("ParseRemoteURL(%q) = %q, %q; want %q, %q", c.in, o, r, c.wantOwner, c.wantRepo)
		}
	}
}

func TestParseRemoteURLRejectsNonGitHub(t *testing.T) {
	cases := []string{
		"https://gitlab.com/foo/bar.git",
		"git@bitbucket.org:foo/bar.git",
		"https://example.com/foo.git",
		"",
		"nonsense",
	}
	for _, c := range cases {
		_, _, err := ParseRemoteURL(c)
		if err == nil {
			t.Errorf("ParseRemoteURL(%q) should have errored", c)
		}
	}
}

func TestParseRemoteURLRejectsMalformed(t *testing.T) {
	cases := []string{
		"https://github.com/",
		"https://github.com/onlyowner",
		"git@github.com:",
		"git@github.com:onlyowner",
	}
	for _, c := range cases {
		_, _, err := ParseRemoteURL(c)
		if err == nil {
			t.Errorf("ParseRemoteURL(%q) should have errored", c)
		}
	}
}

func TestResolveIssueRefFullURLPassthrough(t *testing.T) {
	url, owner, repo, num, err := ResolveIssueRef("https://github.com/aisu-ai/aidev/issues/42", "")
	if err != nil {
		t.Fatal(err)
	}
	if url != "https://github.com/aisu-ai/aidev/issues/42" {
		t.Errorf("url = %q", url)
	}
	if owner != "aisu-ai" || repo != "aidev" || num != 42 {
		t.Errorf("got %q/%q#%d", owner, repo, num)
	}
}

func TestResolveIssueRefBadURLReturnsError(t *testing.T) {
	_, _, _, _, err := ResolveIssueRef("https://github.com/onlyowner", "")
	if err == nil {
		t.Error("expected error")
	}
}

func TestResolveIssueRefEmpty(t *testing.T) {
	_, _, _, _, err := ResolveIssueRef("", "")
	if err == nil {
		t.Error("expected error")
	}
}

func TestResolveIssueRefBareNumberWithoutRepoErrors(t *testing.T) {
	_, _, _, _, err := ResolveIssueRef("533", "")
	if err == nil {
		t.Error("expected error when repo root is empty")
	}
}

func TestResolveIssueRefBareNumberNonNumericErrors(t *testing.T) {
	_, _, _, _, err := ResolveIssueRef("foo", "/tmp")
	if err == nil {
		t.Error("expected error for non-numeric bare ref")
	}
}

func TestResolveIssueRefBareNumberNegativeErrors(t *testing.T) {
	_, _, _, _, err := ResolveIssueRef("0", "/tmp")
	if err == nil {
		t.Error("expected error for non-positive issue number")
	}
	_, _, _, _, err = ResolveIssueRef("-5", "/tmp")
	if err == nil {
		t.Error("expected error for negative issue number")
	}
}

// TestResolveIssueRefBareNumberInferFromGit exercises the happy path
// by creating a throwaway git repo with a real remote and asking
// ResolveIssueRef to turn "533" into a full URL. Requires git on
// PATH; skips otherwise (e.g. minimal CI containers).
func TestResolveIssueRefBareNumberInferFromGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available on PATH")
	}

	dir := t.TempDir()

	// git init, then set an https remote.
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("remote", "add", "origin", "https://github.com/aisu-ai/aidev.git")

	url, owner, repo, num, err := ResolveIssueRef("533", dir)
	if err != nil {
		t.Fatal(err)
	}
	if url != "https://github.com/aisu-ai/aidev/issues/533" {
		t.Errorf("url = %q", url)
	}
	if owner != "aisu-ai" || repo != "aidev" || num != 533 {
		t.Errorf("got %q/%q#%d", owner, repo, num)
	}
}

// TestResolveIssueRefBareNumberSSHRemote verifies the SSH remote
// form also works for bare-number resolution.
func TestResolveIssueRefBareNumberSSHRemote(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available on PATH")
	}

	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("remote", "add", "origin", "git@github.com:AiSU-AI/Company-Site.git")

	_, owner, repo, num, err := ResolveIssueRef("42", dir)
	if err != nil {
		t.Fatal(err)
	}
	if owner != "AiSU-AI" || repo != "Company-Site" || num != 42 {
		t.Errorf("got %q/%q#%d", owner, repo, num)
	}
}

func TestInferOwnerRepoFromGitMissingRepoErrors(t *testing.T) {
	dir := t.TempDir()
	// Not a git repo at all — git config should fail.
	_, _, err := InferOwnerRepoFromGit(dir)
	if err == nil {
		t.Error("expected error in non-git dir")
	}
}

func TestInferOwnerRepoFromGitMissingRemoteErrors(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available on PATH")
	}
	dir := t.TempDir()
	cmd := exec.Command("git", "init", "-q")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	// Repo exists but has no remote.origin.url set.
	_, _, err := InferOwnerRepoFromGit(dir)
	if err == nil {
		t.Error("expected error when remote.origin.url is unset")
	}
	// Sanity check that dir is still there and we didn't leak.
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		t.Errorf("temp .git dir missing: %v", err)
	}
}
