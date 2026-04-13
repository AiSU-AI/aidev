// Package repo walks a target repository on disk and extracts the context
// aidev's Scout agent needs: architectural intent, code layout, and any
// repo-local engineering principles.
//
// The goal of the scout is NOT to read every file — it's to gather the
// smallest set of artifacts that a critic can reason about: README, CLAUDE.md
// or equivalent, architecture docs, the top-level file/directory layout,
// and an inventory of languages in use.
package repo

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Snapshot is the structured view of a repository Scout builds up.
type Snapshot struct {
	Root            string
	Languages       map[string]int // extension -> file count
	TopLevelEntries []string       // names of files/dirs at repo root
	ReadmePath      string
	ReadmeContent   string
	AgentMarkdown   string // CLAUDE.md, AGENTS.md, or AIDEV.md if present
	ArchitectureDoc string // ARCHITECTURE.md if present
	CharterPath     string // .aidev/charter.md if present
	CharterContent  string // contents of CharterPath
	ClarifierPath   string // .aidev/clarifier.md if present
	ClarifierContent string // contents of ClarifierPath
	PrinciplesPath  string // .aidev/principles.yaml if present
	TotalFiles      int
}

// HasStrongSignal reports whether the snapshot has at least one source
// of truth the Critic can anchor against: a README, an agent markdown, an
// architecture doc, or a charter. Returns false when all four are absent,
// at which point the main entry point nudges the user to run `aidev
// charter`.
func (s *Snapshot) HasStrongSignal() bool {
	if s == nil {
		return false
	}
	if len(s.ReadmeContent) >= 100 {
		return true
	}
	if s.AgentMarkdown != "" {
		return true
	}
	if s.ArchitectureDoc != "" {
		return true
	}
	if s.CharterContent != "" {
		return true
	}
	return false
}

// Scan produces a Snapshot rooted at root. It honours .gitignore-ish sense
// by skipping well-known vendor directories without fully parsing .gitignore —
// the cost of that is a few hundred extra directory walks in the worst case,
// and the benefit is zero extra dependencies.
func Scan(root string) (*Snapshot, error) {
	if root == "" {
		return nil, errors.New("repo: empty root")
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New("repo: root is not a directory")
	}

	snap := &Snapshot{
		Root:      root,
		Languages: make(map[string]int),
	}

	// Top-level entries.
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		snap.TopLevelEntries = append(snap.TopLevelEntries, e.Name())
	}

	// Well-known files at root.
	pickFirst := func(names ...string) string {
		for _, n := range names {
			p := filepath.Join(root, n)
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
		return ""
	}
	if p := pickFirst("README.md", "README", "README.rst"); p != "" {
		snap.ReadmePath = p
		if data, err := os.ReadFile(p); err == nil {
			snap.ReadmeContent = string(data)
		}
	}
	if p := pickFirst("CLAUDE.md", "AGENTS.md", "AIDEV.md"); p != "" {
		if data, err := os.ReadFile(p); err == nil {
			snap.AgentMarkdown = string(data)
		}
	}
	if p := pickFirst("ARCHITECTURE.md", "docs/ARCHITECTURE.md"); p != "" {
		if data, err := os.ReadFile(p); err == nil {
			snap.ArchitectureDoc = string(data)
		}
	}
	if p := pickFirst(".aidev/charter.md"); p != "" {
		snap.CharterPath = p
		if data, err := os.ReadFile(p); err == nil {
			snap.CharterContent = string(data)
		}
	}
	if p := pickFirst(".aidev/clarifier.md"); p != "" {
		snap.ClarifierPath = p
		if data, err := os.ReadFile(p); err == nil {
			snap.ClarifierContent = string(data)
		}
	}
	if p := pickFirst(".aidev/principles.yaml", "config/principles.yaml"); p != "" {
		snap.PrinciplesPath = p
	}

	// Walk the tree, counting files by extension. Skip obvious vendor dirs.
	skipDirs := map[string]bool{
		".git": true, "node_modules": true, "vendor": true, "dist": true,
		"build": true, "target": true, ".next": true, ".venv": true, "venv": true,
		"__pycache__": true, ".aidev-cache": true,
	}

	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// A permissions error on one dir should not abort the whole scan.
			if errors.Is(err, fs.ErrPermission) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			if path == root {
				return nil
			}
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		snap.TotalFiles++
		ext := strings.ToLower(filepath.Ext(d.Name()))
		if ext == "" {
			return nil
		}
		snap.Languages[ext]++
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}

	return snap, nil
}

// TopLanguages returns the n most common file extensions in the snapshot,
// sorted by count descending, as "ext (count)" strings.
func (s *Snapshot) TopLanguages(n int) []string {
	type kv struct {
		ext string
		n   int
	}
	pairs := make([]kv, 0, len(s.Languages))
	for k, v := range s.Languages {
		pairs = append(pairs, kv{k, v})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].n > pairs[j].n })
	if n > len(pairs) {
		n = len(pairs)
	}
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, pairs[i].ext+" ("+itoa(pairs[i].n)+")")
	}
	return out
}

func itoa(n int) string {
	// Tiny helper to keep the import list lean; strconv is fine too but this
	// avoids pulling it into a file that otherwise wouldn't need it.
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
