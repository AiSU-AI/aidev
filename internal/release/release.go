// Package release provides aidev's self-update mechanism: fetching
// release metadata from the GitHub Releases REST API, downloading the
// platform-specific tarball, and atomically swapping the on-disk binary.
//
// The public surface is small enough that `aidev upgrade` and
// `aidev ls` each compose one or two calls from this package; tests can
// exercise the API client against an httptest.Server without hitting
// real network.
package release

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// DefaultBaseURL is the public GitHub API origin. Tests inject a
// httptest.Server URL by passing it to FetchReleases / FetchLatest.
const DefaultBaseURL = "https://api.github.com"

// DefaultOwner / DefaultRepo identify the canonical aidev repo on
// GitHub. Subcommands pass these through; tests use the same.
const (
	DefaultOwner = "AiSU-AI"
	DefaultRepo  = "aidev"
)

// Release is the trimmed-down release metadata we surface to callers.
// We deliberately drop fields like uploader, assets_url, etc. — the
// caller's mental model is "what tag, what assets, when did it ship".
type Release struct {
	Tag         string    `json:"tag_name"`
	Name        string    `json:"name"`
	PublishedAt time.Time `json:"published_at"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	Notes       string    `json:"body"`
	Assets      []Asset   `json:"assets"`
}

// Asset is one downloadable file attached to a Release.
type Asset struct {
	Name        string `json:"name"`
	DownloadURL string `json:"browser_download_url"`
	Size        int64  `json:"size"`
}

// FetchOptions tunes the HTTP client without exposing the raw client
// to callers. BaseURL exists only to make httptest injection possible.
type FetchOptions struct {
	BaseURL string        // defaults to DefaultBaseURL
	Timeout time.Duration // defaults to 15s
}

// FetchReleases returns at most `limit` releases for owner/repo,
// newest first. The GitHub /releases endpoint already orders by
// created_at desc, so we just respect that order.
//
// Drafts are filtered out — they're not installable via tag download.
// Prereleases ARE returned (some users want to opt into them) but the
// caller decides what to do with them.
func FetchReleases(ctx context.Context, owner, repo string, limit int, opts *FetchOptions) ([]Release, error) {
	base, client := resolveClient(opts)
	if limit <= 0 {
		limit = 30
	}
	url := fmt.Sprintf("%s/repos/%s/%s/releases?per_page=%d", base, owner, repo, limit)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("release: build request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "aidev")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("release: GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("release: GET %s -> HTTP %d: %s", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var all []Release
	if err := json.NewDecoder(resp.Body).Decode(&all); err != nil {
		return nil, fmt.Errorf("release: decode response: %w", err)
	}

	// Drop drafts. The order from GitHub is newest-first; preserve it.
	out := make([]Release, 0, len(all))
	for _, r := range all {
		if r.Draft {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// FetchLatest returns the release tagged "Latest" on GitHub — NOT
// simply the newest in time. Maintainers can pin "Latest" to an older
// tag if a newer one is prerelease-only.
func FetchLatest(ctx context.Context, owner, repo string, opts *FetchOptions) (*Release, error) {
	base, client := resolveClient(opts)
	url := fmt.Sprintf("%s/repos/%s/%s/releases/latest", base, owner, repo)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("release: build request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "aidev")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("release: GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("release: no latest release published for %s/%s yet", owner, repo)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("release: GET %s -> HTTP %d: %s", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var r Release
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, fmt.Errorf("release: decode response: %w", err)
	}
	return &r, nil
}

// FetchByTag returns the release with the given tag, e.g. "v0.5.0".
func FetchByTag(ctx context.Context, owner, repo, tag string, opts *FetchOptions) (*Release, error) {
	base, client := resolveClient(opts)
	url := fmt.Sprintf("%s/repos/%s/%s/releases/tags/%s", base, owner, repo, tag)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("release: build request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "aidev")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("release: GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("release: no release found with tag %q for %s/%s", tag, owner, repo)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("release: GET %s -> HTTP %d: %s", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var r Release
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, fmt.Errorf("release: decode response: %w", err)
	}
	return &r, nil
}

// AssetFor returns the asset matching the given GOOS/GOARCH using the
// canonical `aidev-<tag>-<goos>-<goarch>.tar.gz` naming convention from
// .github/workflows/release.yaml. Returns an error when no asset
// matches — typically because the release workflow hasn't yet
// published a build for the user's platform.
func (r *Release) AssetFor(goos, goarch string) (*Asset, error) {
	want := fmt.Sprintf("aidev-%s-%s-%s.tar.gz", r.Tag, goos, goarch)
	for i := range r.Assets {
		if r.Assets[i].Name == want {
			return &r.Assets[i], nil
		}
	}
	have := make([]string, 0, len(r.Assets))
	for _, a := range r.Assets {
		have = append(have, a.Name)
	}
	return nil, fmt.Errorf("release: no asset named %q in release %s (have: %s)", want, r.Tag, strings.Join(have, ", "))
}

// CurrentPlatformAsset is a convenience wrapper for AssetFor using
// runtime.GOOS/GOARCH — the typical caller path.
func (r *Release) CurrentPlatformAsset() (*Asset, error) {
	return r.AssetFor(runtime.GOOS, runtime.GOARCH)
}

// Download fetches the asset's tarball to a fresh temp file. Returns
// the path. Caller is responsible for cleanup (and almost always
// follows with Extract → SwapBinary).
func Download(ctx context.Context, asset *Asset, opts *FetchOptions) (string, error) {
	if asset == nil {
		return "", errors.New("release: nil asset")
	}
	_, client := resolveClient(opts)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, asset.DownloadURL, nil)
	if err != nil {
		return "", fmt.Errorf("release: build request: %w", err)
	}
	req.Header.Set("User-Agent", "aidev")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("release: GET %s: %w", asset.DownloadURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("release: GET %s -> HTTP %d", asset.DownloadURL, resp.StatusCode)
	}

	tmp, err := os.CreateTemp("", "aidev-asset-*.tar.gz")
	if err != nil {
		return "", fmt.Errorf("release: create temp file: %w", err)
	}
	defer tmp.Close()

	if _, err := io.Copy(tmp, resp.Body); err != nil {
		os.Remove(tmp.Name())
		return "", fmt.Errorf("release: write asset: %w", err)
	}
	return tmp.Name(), nil
}

// Extract unpacks the given tarball into a temp dir and returns the
// path to the embedded `aidev` binary. The release tarball has shape
// `aidev-<tag>-<goos>-<goarch>/aidev` per .github/workflows/release.yaml.
func Extract(tgzPath string) (string, error) {
	f, err := os.Open(tgzPath)
	if err != nil {
		return "", fmt.Errorf("release: open tarball: %w", err)
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", fmt.Errorf("release: gzip: %w", err)
	}
	defer gz.Close()

	dir, err := os.MkdirTemp("", "aidev-extract-*")
	if err != nil {
		return "", fmt.Errorf("release: mkdir temp: %w", err)
	}

	tr := tar.NewReader(gz)
	var binPath string
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("release: tar read: %w", err)
		}
		// Defense in depth against tar-slip: refuse absolute paths and
		// any `..` segment. The release tarballs we ship don't use
		// either, but the OSS world is full of malicious tarballs and
		// we don't want to be the next vuln.
		if filepath.IsAbs(hdr.Name) || strings.Contains(hdr.Name, "..") {
			return "", fmt.Errorf("release: refusing entry with unsafe path %q", hdr.Name)
		}
		target := filepath.Join(dir, hdr.Name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return "", err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return "", err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return "", err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return "", err
			}
			out.Close()
			if filepath.Base(target) == "aidev" {
				binPath = target
			}
		}
	}
	if binPath == "" {
		return "", fmt.Errorf("release: extracted archive does not contain an `aidev` binary in %s", dir)
	}
	return binPath, nil
}

// SwapBinary atomically replaces destPath with srcPath. On Unix this
// works even when destPath is the currently-running binary — the OS
// keeps the open inode alive until the running process exits, and the
// rename swaps which file the name resolves to.
//
// Cross-filesystem renames fail with EXDEV on Linux; fall back to copy+
// rename within the destination's directory.
func SwapBinary(srcPath, destPath string) error {
	// Try a same-dir staged rename first: copy src to a sibling of dest,
	// then rename. This guarantees same-filesystem semantics regardless
	// of whether srcPath is in /tmp.
	stage := destPath + ".aidev-upgrade.tmp"
	if err := copyFile(srcPath, stage, 0o755); err != nil {
		return fmt.Errorf("release: stage: %w", err)
	}
	if err := os.Rename(stage, destPath); err != nil {
		// Best-effort cleanup of the stage file before bubbling the
		// error — leaving it dangling makes the next attempt fail with
		// a confusing "file exists" message.
		_ = os.Remove(stage)
		return fmt.Errorf("release: swap %s -> %s: %w", stage, destPath, err)
	}
	return nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func resolveClient(opts *FetchOptions) (string, *http.Client) {
	base := DefaultBaseURL
	timeout := 15 * time.Second
	if opts != nil {
		if opts.BaseURL != "" {
			base = opts.BaseURL
		}
		if opts.Timeout > 0 {
			timeout = opts.Timeout
		}
	}
	return base, &http.Client{Timeout: timeout}
}
