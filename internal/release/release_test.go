package release

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeServer returns an httptest.Server that responds to the three
// endpoints we hit: GET /repos/.../releases, /releases/latest,
// /releases/tags/<tag>. The test passes the server's URL via
// FetchOptions.BaseURL so no real network is touched.
func fakeServer(t *testing.T) *httptest.Server {
	t.Helper()
	releases := []Release{
		{
			Tag:         "v0.5.0",
			Name:        "v0.5.0",
			PublishedAt: time.Date(2026, 5, 14, 5, 41, 0, 0, time.UTC),
			Notes:       "Reviewer + Triage improvements",
			Assets: []Asset{
				{Name: "aidev-v0.5.0-darwin-arm64.tar.gz", DownloadURL: "https://example.invalid/v0.5.0/darwin-arm64", Size: 12345},
				{Name: "aidev-v0.5.0-darwin-amd64.tar.gz", DownloadURL: "https://example.invalid/v0.5.0/darwin-amd64", Size: 12345},
				{Name: "aidev-v0.5.0-linux-amd64.tar.gz", DownloadURL: "https://example.invalid/v0.5.0/linux-amd64", Size: 12345},
				{Name: "aidev-v0.5.0-linux-arm64.tar.gz", DownloadURL: "https://example.invalid/v0.5.0/linux-arm64", Size: 12345},
			},
		},
		{
			Tag:         "v0.4.0",
			Name:        "v0.4.0",
			PublishedAt: time.Date(2026, 4, 19, 15, 46, 0, 0, time.UTC),
			Notes:       "earlier release",
			Assets: []Asset{
				{Name: "aidev-v0.4.0-darwin-arm64.tar.gz", DownloadURL: "https://example.invalid/v0.4.0/darwin-arm64", Size: 12345},
			},
		},
		{
			Tag:         "v0.3.0-beta",
			Name:        "v0.3.0-beta",
			PublishedAt: time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
			Prerelease:  true,
			Notes:       "early prerelease",
			Assets:      nil,
		},
		{
			// Draft — must be filtered out by FetchReleases.
			Tag:         "v0.6.0-draft",
			Name:        "v0.6.0-draft",
			PublishedAt: time.Date(2026, 5, 14, 0, 0, 0, 0, time.UTC),
			Draft:       true,
			Assets:      nil,
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/AiSU-AI/aidev/releases", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(releases)
	})
	mux.HandleFunc("/repos/AiSU-AI/aidev/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		// Pretend GitHub considers v0.5.0 the "Latest" tag.
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(releases[0])
	})
	mux.HandleFunc("/repos/AiSU-AI/aidev/releases/tags/v0.4.0", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(releases[1])
	})
	mux.HandleFunc("/repos/AiSU-AI/aidev/releases/tags/v9.9.9", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	return httptest.NewServer(mux)
}

func TestFetchReleasesReturnsNonDraftsNewestFirst(t *testing.T) {
	srv := fakeServer(t)
	defer srv.Close()

	got, err := FetchReleases(context.Background(), "AiSU-AI", "aidev", 30, &FetchOptions{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("FetchReleases: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 releases (drafts filtered), got %d", len(got))
	}
	wantOrder := []string{"v0.5.0", "v0.4.0", "v0.3.0-beta"}
	for i, r := range got {
		if r.Tag != wantOrder[i] {
			t.Errorf("release[%d].Tag = %q, want %q", i, r.Tag, wantOrder[i])
		}
	}
	for _, r := range got {
		if r.Draft {
			t.Errorf("draft release leaked through: %s", r.Tag)
		}
	}
}

func TestFetchLatestReturnsTaggedLatest(t *testing.T) {
	srv := fakeServer(t)
	defer srv.Close()

	r, err := FetchLatest(context.Background(), "AiSU-AI", "aidev", &FetchOptions{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("FetchLatest: %v", err)
	}
	if r.Tag != "v0.5.0" {
		t.Errorf("FetchLatest.Tag = %q, want v0.5.0", r.Tag)
	}
}

func TestFetchByTagReturnsRequested(t *testing.T) {
	srv := fakeServer(t)
	defer srv.Close()

	r, err := FetchByTag(context.Background(), "AiSU-AI", "aidev", "v0.4.0", &FetchOptions{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("FetchByTag: %v", err)
	}
	if r.Tag != "v0.4.0" {
		t.Errorf("FetchByTag.Tag = %q, want v0.4.0", r.Tag)
	}
}

func TestFetchByTagReturnsClearErrorOnUnknownTag(t *testing.T) {
	srv := fakeServer(t)
	defer srv.Close()

	_, err := FetchByTag(context.Background(), "AiSU-AI", "aidev", "v9.9.9", &FetchOptions{BaseURL: srv.URL})
	if err == nil {
		t.Fatal("expected error for unknown tag")
	}
	if !strings.Contains(err.Error(), "no release found with tag") {
		t.Errorf("error should mention the missing tag, got: %v", err)
	}
}

func TestAssetForMatchesPlatformNamingConvention(t *testing.T) {
	r := &Release{
		Tag: "v0.5.0",
		Assets: []Asset{
			{Name: "aidev-v0.5.0-darwin-arm64.tar.gz"},
			{Name: "aidev-v0.5.0-darwin-amd64.tar.gz"},
			{Name: "aidev-v0.5.0-linux-arm64.tar.gz"},
			{Name: "aidev-v0.5.0-linux-amd64.tar.gz"},
		},
	}
	cases := []struct {
		goos, goarch string
		wantName     string
	}{
		{"darwin", "arm64", "aidev-v0.5.0-darwin-arm64.tar.gz"},
		{"darwin", "amd64", "aidev-v0.5.0-darwin-amd64.tar.gz"},
		{"linux", "amd64", "aidev-v0.5.0-linux-amd64.tar.gz"},
		{"linux", "arm64", "aidev-v0.5.0-linux-arm64.tar.gz"},
	}
	for _, c := range cases {
		t.Run(c.goos+"-"+c.goarch, func(t *testing.T) {
			a, err := r.AssetFor(c.goos, c.goarch)
			if err != nil {
				t.Fatalf("AssetFor(%s,%s): %v", c.goos, c.goarch, err)
			}
			if a.Name != c.wantName {
				t.Errorf("got %q, want %q", a.Name, c.wantName)
			}
		})
	}
}

func TestAssetForReturnsHelpfulErrorWhenPlatformMissing(t *testing.T) {
	r := &Release{
		Tag: "v0.5.0",
		Assets: []Asset{
			{Name: "aidev-v0.5.0-linux-amd64.tar.gz"},
		},
	}
	_, err := r.AssetFor("windows", "arm64")
	if err == nil {
		t.Fatal("expected error for missing platform")
	}
	if !strings.Contains(err.Error(), "windows-arm64") {
		t.Errorf("error should name the missing platform, got: %v", err)
	}
	if !strings.Contains(err.Error(), "linux-amd64") {
		t.Errorf("error should list available assets, got: %v", err)
	}
}

func TestExtractFindsAidevBinaryInsideExpectedTarShape(t *testing.T) {
	// Build a tarball matching .github/workflows/release.yaml shape:
	// aidev-<tag>-<goos>-<goarch>/aidev
	tgz := buildFakeTarball(t, "aidev-v0.5.0-darwin-arm64", "#!/bin/sh\necho hello\n")

	bin, err := Extract(tgz)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if filepath.Base(bin) != "aidev" {
		t.Errorf("returned path basename = %q, want aidev", filepath.Base(bin))
	}
	data, err := os.ReadFile(bin)
	if err != nil {
		t.Fatalf("read extracted bin: %v", err)
	}
	if !strings.Contains(string(data), "hello") {
		t.Errorf("extracted bin contents lost: %q", string(data))
	}
	info, err := os.Stat(bin)
	if err != nil {
		t.Fatalf("stat extracted bin: %v", err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Errorf("extracted bin is not executable: %v", info.Mode())
	}
}

func TestExtractRejectsTarSlipPaths(t *testing.T) {
	tgz := buildFakeTarballRaw(t, []tarEntry{
		{name: "../escape/aidev", body: "evil"},
	})
	_, err := Extract(tgz)
	if err == nil {
		t.Fatal("expected error for path traversal")
	}
	if !strings.Contains(err.Error(), "unsafe path") {
		t.Errorf("error should call out the unsafe path, got: %v", err)
	}
}

func TestExtractFailsWhenNoAidevBinaryPresent(t *testing.T) {
	tgz := buildFakeTarballRaw(t, []tarEntry{
		{name: "aidev-v0.5.0-darwin-arm64/README.md", body: "no binary here"},
	})
	_, err := Extract(tgz)
	if err == nil {
		t.Fatal("expected error when binary is missing")
	}
	if !strings.Contains(err.Error(), "does not contain an `aidev` binary") {
		t.Errorf("error should explain the missing binary, got: %v", err)
	}
}

func TestSwapBinaryReplacesDestAtomically(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "aidev")
	if err := os.WriteFile(dest, []byte("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "aidev-new")
	if err := os.WriteFile(src, []byte("NEW"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := SwapBinary(src, dest); err != nil {
		t.Fatalf("SwapBinary: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "NEW" {
		t.Errorf("dest contents = %q, want NEW", string(got))
	}
	// The stage file (.aidev-upgrade.tmp) should not be left around
	// after a successful swap — it gets renamed.
	if _, err := os.Stat(dest + ".aidev-upgrade.tmp"); !os.IsNotExist(err) {
		t.Errorf("stage file should not exist after successful swap")
	}
}

// ---- test helpers --------------------------------------------------

type tarEntry struct {
	name string
	body string
}

func buildFakeTarball(t *testing.T, dirName, binBody string) string {
	t.Helper()
	return buildFakeTarballRaw(t, []tarEntry{
		{name: dirName + "/", body: ""},
		{name: dirName + "/aidev", body: binBody},
	})
}

func buildFakeTarballRaw(t *testing.T, entries []tarEntry) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: 0o755, Size: int64(len(e.body))}
		if strings.HasSuffix(e.name, "/") {
			hdr.Typeflag = tar.TypeDir
			hdr.Size = 0
		} else {
			hdr.Typeflag = tar.TypeReg
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("tar header %q: %v", e.name, err)
		}
		if hdr.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatalf("tar body %q: %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "fake.tar.gz")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}
