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
//
// maxFileBytes is 128 KiB — large enough to fit a full locale JSON, a
// large React component, or a non-trivial Go source file without
// truncation. Truncation was the root cause of the v0.3a.1
// "placeholder non-English translations" regression: the Implementer
// saw a clipped copy of en/common.json and couldn't resolve the
// canonical "Contact Sales" value in any other locale, so it
// fabricated placeholders instead. 128 KiB eliminates that failure
// mode for every realistic source file we've seen; anything beyond
// that almost certainly shouldn't be in a single diff anyway.
//
// maxRequestedFiles is 40 — raised from 15 so a consolidation task
// across N locales + M call sites still fits in the picker's budget.
// Taskes that need more than 40 files should almost certainly be
// split by the Architect into multiple sketches.
//
// maxNeedFilesRounds is the cap on the iterative NEED_FILES loop
// (see generateDiff). Two extra rounds beyond the initial picker
// means up to 3 generateDiff LLM calls per implementer run in the
// worst case, which keeps cost predictable while giving the model
// enough headroom to discover it needs files the picker missed.
const (
	maxRequestedFiles  = 40
	maxFileBytes       = 128 * 1024
	maxNeedFilesRounds = 2

	// inventoryCap is the maximum number of repo-relative file paths
	// shown to the picker and to generateDiff's "file inventory"
	// section. Bumped from 2000 to 3000 after a real monorepo
	// (Next.js marketing site inside a larger product monorepo) hit
	// the cap on alphabetically-earlier directories and never reached
	// apps/marketing/src/components/ — leaving the Implementer with
	// a truncated view of the tree and no TSX files from the
	// component directory the Clarifier had explicitly cited. 3000
	// is the next belt-and-braces step above 2000; the FUNDAMENTAL
	// fix is in Run(), which now auto-harvests paths from upstream
	// agent output (Scout, Critic, Clarifier, sketch) and loads
	// them directly regardless of whether they're in the inventory.
	// The inventory is now a hint, not a source of truth.
	inventoryCap = 3000
)

// Run asks the LLM to produce a unified diff that implements the chosen
// Sketch. v0.3a.2 drives an **iterative agentic loop** on top of the
// existing single-shot Provider interface:
//
//	Turn 1: send the full context chain plus a file inventory and ask
//	        the model to list (as a JSON array) the files it needs to
//	        see the contents of to write a correct patch.
//	Turn 2: read those files from disk, embed their contents in the
//	        prompt, and ask for the unified diff.
//	Turn 2b (up to maxNeedFilesRounds times): if the model responds
//	        with `NEED_FILES: [...]` instead of a diff, load those
//	        files, merge them with the already-loaded contents, and
//	        re-ask. This is the poor-man's tool-use loop aidev uses
//	        until the llm.Provider interface gains native tool calls.
//
// The loop is bounded — no runaway cost, no accidental hallucination
// of fresh requests — and every response must eventually terminate at
// either `diff --git` (success) or `ERROR:` (hard fail). Silent punts
// like `# no-op`, placeholder strings, or "I wrote a TODO.md for you"
// are not allowed by the prompt and are rejected by the validator.
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

	// Pre-load files that upstream agents (Scout, Critic, Clarifier,
	// Architect) have already cited in their output. These paths are
	// authoritative: if the Clarifier quotes
	// `apps/marketing/src/components/sections/foo/bar.tsx:193` as
	// evidence, we should load that file automatically instead of
	// making the Implementer guess from the picker's inventory.
	//
	// This is the fix for the v0.3a.3 failure mode where the
	// Implementer emitted `ERROR: files don't exist` because its
	// inventory was truncated before reaching the directory the
	// Clarifier had literally just cited. The inventory is
	// necessarily bounded; upstream agent output is not, and it
	// tends to contain exactly the paths that matter.
	contents := make(map[string]string)
	var missing []string
	upstreamPaths := harvestPaths(
		c.Issue.Body,
		c.ScoutReport,
		c.CriticReport,
		chosen.Markdown,
		c.ClarifierNotes,
	)
	if c.Snapshot != nil {
		upstreamPaths = append(upstreamPaths, harvestPaths(c.Snapshot.ClarifierContent)...)
	}
	if len(upstreamPaths) > 0 {
		harvested, harvestMissing := readFiles(c.Snapshot.Root, upstreamPaths)
		for p, body := range harvested {
			contents[p] = body
		}
		// harvestMissing represents upstream citations that
		// couldn't be resolved — worth surfacing so the
		// Implementer knows the Clarifier/Scout mentioned them
		// but the harness couldn't find them on disk. This is
		// useful feedback even though it's rare (upstream agents
		// usually cite real paths).
		missing = appendUniqueCapped(missing, harvestMissing, 40)
	}

	// Turn 1: ask the model which files it needs to see.
	wanted, err := i.selectFiles(ctx, c, chosen)
	if err != nil {
		return nil, fmt.Errorf("implementer: file selection: %w", err)
	}

	// Load the picker's requested file contents on top of the
	// already-harvested upstream paths. Both go into the same
	// contents map.
	pickerContents, pickerMissing := readFiles(c.Snapshot.Root, wanted)
	for p, body := range pickerContents {
		contents[p] = body
	}
	missing = appendUniqueCapped(missing, pickerMissing, 40)

	// Turn 2 (with up to maxNeedFilesRounds additional rounds): ask
	// for the diff. If the model comes back with NEED_FILES instead,
	// load the requested paths and retry with the expanded context.
	//
	// missing is threaded through every round so the model always
	// knows exactly which paths it guessed wrong on the previous
	// turn. Without this feedback the model keeps asking for
	// hallucinated paths, burns the NEED_FILES budget, and the run
	// dies with "NEED_FILES limit reached" while the real paths
	// sit in the inventory unread.
	for round := 0; round <= maxNeedFilesRounds; round++ {
		patch, need, err := i.generateDiff(ctx, c, chosen, contents, missing)
		if err != nil {
			return nil, err
		}
		if patch != nil {
			return patch, nil
		}
		// The model responded with NEED_FILES. If we're already at
		// the loop cap, fail loudly instead of quietly returning a
		// half-cooked diff — the prompt promised a real diff and
		// we're not going to let it slide.
		if round == maxNeedFilesRounds {
			return nil, fmt.Errorf("implementer: NEED_FILES limit reached (%d rounds) — model still requesting %v", maxNeedFilesRounds, need)
		}
		// De-duplicate against files already loaded so the model can't
		// just keep asking for the same path over and over.
		fresh := make([]string, 0, len(need))
		for _, p := range need {
			if _, have := contents[p]; have {
				continue
			}
			fresh = append(fresh, p)
		}
		if len(fresh) == 0 {
			return nil, fmt.Errorf("implementer: NEED_FILES requested only files that were already provided: %v", need)
		}
		more, missedNow := readFiles(c.Snapshot.Root, fresh)
		// Update the missing list for the next turn: keep both the
		// paths we couldn't load this round AND the paths we've
		// failed to load on earlier rounds (capped at a reasonable
		// size so the prompt doesn't balloon).
		missing = appendUniqueCapped(missing, missedNow, 40)
		if len(more) == 0 {
			// Every path the model asked for on this round was
			// missing. We don't fail here any more — missingNow is
			// now in the prompt, so next round the model has the
			// actual feedback signal it needs to try again. But we
			// still don't let the loop be infinite — the round cap
			// above covers that.
			continue
		}
		for p, body := range more {
			contents[p] = body
		}
	}
	// Unreachable — the loop above either returns a patch or errors.
	return nil, errors.New("implementer: loop fell through unexpectedly")
}

// appendUniqueCapped appends every string in extras to base that
// isn't already there, and returns a slice capped at cap elements
// (dropping oldest entries first). Used by the NEED_FILES loop to
// track missing-path history across rounds without unbounded growth.
func appendUniqueCapped(base, extras []string, cap int) []string {
	seen := make(map[string]bool, len(base))
	for _, s := range base {
		seen[s] = true
	}
	out := base
	for _, s := range extras {
		if !seen[s] {
			out = append(out, s)
			seen[s] = true
		}
	}
	if len(out) > cap {
		out = out[len(out)-cap:]
	}
	return out
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

	files := listSourceFiles(c.Snapshot.Root, inventoryCap)
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
// repoRoot. Files larger than maxFileBytes are truncated with a
// marker at the end. Returns two values:
//
//  1. a map of successfully-loaded content, keyed by the ORIGINAL
//     requested path (not the resolved absolute path), so the
//     second-turn prompt can cite the same strings the first turn
//     returned.
//
//  2. a slice of paths that were requested but could NOT be loaded
//     because they don't exist on disk, are directories, or failed
//     to read. The caller surfaces this list back to the model on
//     the next turn so it knows its guess was wrong — without this
//     feedback signal, a model hallucinating paths will silently
//     get empty responses and keep guessing until the NEED_FILES
//     cap trips.
//
// Paths that fail isSafeRelPath are silently dropped (rejected at
// the boundary, not reported). We never surface "you asked for
// ../../etc/passwd" to the model because that's a red-flag path
// that should be killed at the edge.
func readFiles(repoRoot string, paths []string) (map[string]string, []string) {
	out := make(map[string]string, len(paths))
	var missing []string
	for _, p := range paths {
		if !isSafeRelPath(p) {
			continue
		}
		abs := filepath.Join(repoRoot, p)
		info, err := os.Stat(abs)
		if err != nil || info.IsDir() {
			missing = append(missing, p)
			continue
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			missing = append(missing, p)
			continue
		}
		if len(data) > maxFileBytes {
			data = append(data[:maxFileBytes], []byte("\n// ...[truncated by aidev at "+fmt.Sprint(maxFileBytes)+" bytes]...")...)
		}
		out[p] = string(data)
	}
	return out, missing
}

// generateDiff is turn 2. Returns (patch, nil, nil) on success, or
// (nil, neededPaths, nil) if the model asked for more files via the
// NEED_FILES protocol, or (nil, nil, err) on any hard failure. The
// Run() loop interprets the three-way return to decide whether to
// retry with expanded file context or surface a terminal error.
//
// The missing argument is the running list of paths the model has
// requested on prior rounds that the harness could not load
// (because they don't exist on disk, are directories, or failed to
// read). The prompt surfaces these to the model under a "DO NOT
// REQUEST AGAIN" heading so it stops guessing at paths that aren't
// in the repo.
//
// Kept separate from selectFiles so tests and callers can exercise
// the two phases independently.
func (i *Implementer) generateDiff(ctx context.Context, c *Context, chosen *Sketch, contents map[string]string, missing []string) (*Patch, []string, error) {

	system := "You are the Implementer for aidev, a multi-agent coding tool.\n" +
		"\n" +
		"You are a senior engineer pairing with a teammate. The developer has\n" +
		"chosen one of the Architect's sketches and is asking you to do the\n" +
		"work — not to write a checklist, not to hand back a draft for the\n" +
		"human to finish, not to file a TODO. Produce a UNIFIED GIT DIFF\n" +
		"that implements the sketch against the target repository, and make\n" +
		"it complete, valid, and ready to apply.\n" +
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
		"3. Keep the diff MINIMAL in SCOPE but COMPLETE in EXECUTION. Only\n" +
		"   include files the chosen sketch actually needs to touch, but\n" +
		"   within that scope do the whole job — do not leave dangling\n" +
		"   references, half-migrated call sites, or unfinished locale\n" +
		"   rollouts. A diff that covers 8 of 9 locales is a regression.\n" +
		"\n" +
		"4. Include docstrings / comments where the sketch implies new public\n" +
		"   API. Follow the repository's existing conventions (go doc comments,\n" +
		"   python docstrings, etc. as appropriate).\n" +
		"\n" +
		"5. Include tests for every meaningful behaviour the diff adds. Tests\n" +
		"   belong in the same diff, not a follow-up.\n" +
		"\n" +
		"6. Do NOT wrap the diff in a Markdown code fence. Do NOT prefix the\n" +
		"   diff with commentary. The first line of your response MUST be\n" +
		"   EXACTLY ONE of:\n" +
		"\n" +
		"     'diff --git'     (the common case — a real patch)\n" +
		"     'NEED_FILES:'    (see rule 7)\n" +
		"     'ERROR:'         (see rule 8)\n" +
		"\n" +
		"   Silence, prose apologies, # no-op sentinels, and 'here's a\n" +
		"   starting point' drafts are all rejected by the validator.\n" +
		"\n" +
		"7. NEED_FILES protocol. If you cannot write a COMPLETE, VALID diff\n" +
		"   from the files you've already been shown because the real\n" +
		"   content of some other file is required (for example: you need\n" +
		"   the canonical translation string from fr/common.json to copy\n" +
		"   into a new key, or you need to see the actual call site in a\n" +
		"   .tsx component to anchor the patch correctly), emit the\n" +
		"   following exactly:\n" +
		"\n" +
		"     NEED_FILES: [\"path/one.json\", \"path/two.tsx\"]\n" +
		"\n" +
		"   on a single line — a JSON array of repo-relative paths after\n" +
		"   the 'NEED_FILES:' marker, no prose, no code fence. The harness\n" +
		"   will load those files and re-invoke you with the expanded\n" +
		"   context. You get up to two NEED_FILES rounds, so plan ahead:\n" +
		"   request everything you need in one go, not one file at a time.\n" +
		"\n" +
		"   NEED_FILES is NOT an opt-out from doing the work. It is an\n" +
		"   opt-in to seeing MORE of the codebase before you do the work.\n" +
		"   Do not use it to defer writing the diff entirely.\n" +
		"\n" +
		"   IMPORTANT: paths cited in the Clarifier session, Scout brief,\n" +
		"   Critic report, or Architect sketch are KNOWN TO EXIST in the\n" +
		"   repository — those agents ran against the same checkout you\n" +
		"   are and verified those paths. You can NEED_FILES them even if\n" +
		"   they are not listed in the 'Repository file inventory' section\n" +
		"   below: the inventory may be truncated and absence from it is\n" +
		"   NOT evidence that a file is missing. When in doubt, prefer\n" +
		"   NEED_FILES over ERROR.\n" +
		"\n" +
		"8. ERROR escape hatch. Reserved for the NARROW case where the\n" +
		"   task is genuinely impossible — for example, the sketch\n" +
		"   contradicts itself, or it requires migrations outside this\n" +
		"   repository. Emit a single line starting with 'ERROR: '\n" +
		"   followed by a one-sentence explanation. The orchestrator\n" +
		"   will surface the reason verbatim.\n" +
		"\n" +
		"   Do NOT emit ERROR claiming 'files do not exist' unless you\n" +
		"   have FIRST tried to load them via NEED_FILES and the harness\n" +
		"   reported them in the 'Paths you previously requested that DO\n" +
		"   NOT EXIST' section. Absence from your current view is NOT\n" +
		"   evidence of absence from the repository; the inventory is\n" +
		"   necessarily bounded and you can always request more.\n" +
		"\n" +
		"   Concretely: if the Clarifier or Scout brief mentions a path\n" +
		"   like apps/marketing/src/components/foo/bar.tsx and that path\n" +
		"   is not in your current file contents, your next response\n" +
		"   should be NEED_FILES requesting that path — NOT ERROR\n" +
		"   declaring the file doesn't exist. The harness will tell you\n" +
		"   on the next turn whether the path resolved.\n" +
		"\n" +
		"9. FORBIDDEN OUTPUTS — the following are failure modes, not\n" +
		"   acceptable compromises. Prefer NEED_FILES or ERROR over any of\n" +
		"   them:\n" +
		"\n" +
		"   9a. Creating auxiliary checklist/notes files alongside the real\n" +
		"       change (AIDEV_TODO_*.md, NOTES.md, CHECKLIST.md,\n" +
		"       HUMAN_FOLLOWUP.md). The diff is the work. If a human has\n" +
		"       to finish the job from your output, you have failed. Do\n" +
		"       NOT create helper markdown files to track work you\n" +
		"       decided not to do.\n" +
		"\n" +
		"   9b. Comments in file formats that don't support them. JSON\n" +
		"       does NOT have comments — '// TODO' or '/* ... */' lines\n" +
		"       inside a .json hunk produce invalid JSON and the diff\n" +
		"       won't apply. If you think you need to annotate a JSON\n" +
		"       region, you either request the real content via\n" +
		"       NEED_FILES or emit ERROR. Check the file extension before\n" +
		"       adding any kind of comment. TOML, YAML, Python, Go, TS,\n" +
		"       JS allow comments; JSON, JSON5 (mostly), and CSV do not.\n" +
		"\n" +
		"   9c. Placeholder or fabricated string values. If the diff\n" +
		"       needs a specific translation, brand name, API key name,\n" +
		"       or any content-bearing literal, READ the real value from\n" +
		"       the repo — via the files you already have or via\n" +
		"       NEED_FILES. Never invent 'Kontakt Vertrieb' as a stand-in\n" +
		"       for a German translation that already exists elsewhere in\n" +
		"       the repo. If you genuinely cannot find the real value,\n" +
		"       emit ERROR and explain which file you looked for it in.\n" +
		"\n" +
		"   9d. 'Partial starting points' that expect the human to finish\n" +
		"       the job. You are not generating homework. Finish the\n" +
		"       scope the sketch asked for, end to end. If the scope is\n" +
		"       too large for a clean diff, emit ERROR and explain what\n" +
		"       needs to be split — do not silently hand back half the\n" +
		"       work.\n" +
		"\n" +
		"Stay realistic: the user will apply this diff with 'git apply'\n" +
		"and review it. A diff that applies cleanly and finishes the\n" +
		"scope is worth ten drafts that need human cleanup."

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
		user.WriteString("These are the files already loaded for you. If you need to read additional repo-relative files to produce a COMPLETE and VALID diff, respond with `NEED_FILES: [\"path/a.json\", \"path/b.tsx\"]` per rule 7 and the harness will load them and call you again. Do NOT fabricate values, annotate TODOs inside the diff, or hand back a partial starting point.\n")
	}

	// ALWAYS surface a file inventory, even when contents is
	// populated. On NEED_FILES retries the model needs a list of
	// real paths to pick from — otherwise it guesses, burns the
	// retry budget on hallucinated paths, and the run dies with
	// "NEED_FILES limit reached" while the actual files sit in the
	// inventory unread. Cap at inventoryCap so the prompt can't
	// balloon on huge monorepos, but the cap is generous enough for
	// most codebases (a standard Next.js marketing site has on the
	// order of 500-1500 source files).
	inv := listSourceFiles(c.Snapshot.Root, inventoryCap)
	if len(inv) > 0 {
		user.WriteString("\n\n## Repository file inventory (may be truncated — prefer paths from this list, but the absence of a path here is NOT proof the file is missing)\n\n")
		for _, f := range inv {
			user.WriteString("- ")
			user.WriteString(f)
			user.WriteString("\n")
		}
		if len(inv) >= inventoryCap {
			fmt.Fprintf(&user, "\n_(Inventory truncated at %d files. If you need a file not listed here, guess the path and the harness will tell you if it doesn't exist — but prefer picking from the list above.)_\n", inventoryCap)
		}
	}

	// Thread missing paths back to the model so it stops asking for
	// the same hallucinated paths over and over. This is the
	// feedback signal that was silently missing in v0.3a.2: without
	// it, the model had no way to know its picker+NEED_FILES
	// guesses were wrong, and it just kept guessing.
	if len(missing) > 0 {
		user.WriteString("\n\n## Paths you previously requested that DO NOT EXIST in this repo — DO NOT request these again\n\n")
		for _, m := range missing {
			user.WriteString("- ")
			user.WriteString(m)
			user.WriteString("\n")
		}
		user.WriteString("\nThese paths are NOT in the repository. If you still need more files, pick from the repository file inventory above — do not guess at plausible-sounding paths.\n")
	}

	user.WriteString("\n\n## Principles\n\n")
	for _, p := range c.Principles {
		fmt.Fprintf(&user, "- **%s** — %s\n", p.Name, p.Summary)
	}

	// If the Coordinator reviewed a previous attempt at this sketch
	// and flagged concerns, surface them as the LAST section of the
	// prompt so they're the most recent thing the model sees. The
	// feedback is authoritative — it represents the user's quality
	// bar as enforced by a cloud-backed reviewer — and must be
	// addressed one-by-one before another response is emitted.
	if strings.TrimSpace(c.CoordinatorFeedback) != "" {
		user.WriteString("\n\n## Coordinator feedback on your previous attempt (AUTHORITATIVE — address each item)\n\n")
		user.WriteString(c.CoordinatorFeedback)
		user.WriteString("\n\nRe-emit a COMPLETE diff that addresses every concern above. If any concern requires reading additional files, use NEED_FILES. Do NOT re-submit the same diff with minor tweaks — the Coordinator will catch it again.\n")
	}

	// End-of-prompt format reminder. Models attend more strongly to
	// the most recent context, so repeating the output-format rule
	// here significantly reduces the chance of a prose response when
	// the main system prompt is long. This is the first line of
	// defense; tryGenerateDiff() below adds a format-correction retry
	// for the remaining cases.
	user.WriteString("\n\n---\nFORMAT REMINDER: the FIRST LINE of your response MUST be EXACTLY ONE of:\n  diff --git        (a real unified diff)\n  NEED_FILES: [...] (JSON array of repo-relative paths you need to see)\n  ERROR: <reason>   (genuinely impossible, explain why)\nNo prose preamble. No 'Looking at this, I need...'. No 'Here is the diff:'. The first characters of your response MUST match one of those three prefixes.")

	return i.tryGenerateDiff(ctx, system, user.String(), 0)
}

// maxFormatRetries is how many times we'll nudge the model to use a
// valid output prefix after it produces prose. One retry is enough
// in practice — cloud models comply with a direct 'your previous
// response violated the format, here's what you wrote, here's what
// to do' follow-up almost always. Past one retry, something is
// deeply wrong and further retries waste tokens without fixing it.
const maxFormatRetries = 1

// tryGenerateDiff is the underlying single-shot call. It inspects
// the model's response against the prefix grammar and, if the model
// wrote prose instead of a valid prefix, self-corrects by re-calling
// with a specific follow-up message that quotes the broken response
// back to the model and tells it to use the format. Bounded at
// maxFormatRetries so we can't loop on a model that refuses to
// comply.
func (i *Implementer) tryGenerateDiff(ctx context.Context, system, userMsg string, retries int) (*Patch, []string, error) {
	resp, err := i.Provider.Complete(ctx, llm.Request{
		System: system,
		Messages: []llm.Message{
			{Role: "user", Content: userMsg},
		},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("implementer: %w", err)
	}

	raw := strings.TrimSpace(resp.Content)
	if raw == "" {
		return nil, nil, errors.New("implementer: empty response")
	}
	// Strip a leading markdown code fence if the model ignored the
	// "no code fence" instruction — we've seen it happen in practice.
	raw = stripCodeFence(raw)

	switch {
	case strings.HasPrefix(raw, "NEED_FILES:"):
		// Iterative file request. Parse the JSON array that follows
		// the marker. The caller (Run) loads those files and re-asks.
		need, parseErr := parseNeedFiles(raw)
		if parseErr != nil {
			return nil, nil, fmt.Errorf("implementer: NEED_FILES parse: %w", parseErr)
		}
		if len(need) == 0 {
			return nil, nil, errors.New("implementer: NEED_FILES directive had no paths")
		}
		return nil, need, nil

	case strings.HasPrefix(raw, "ERROR:"):
		// Hard-fail escape hatch. The Implementer has declared it cannot
		// produce a diff at all. Surface the reason verbatim to the
		// orchestrator so the user sees exactly what blocked it.
		reason := strings.TrimSpace(strings.TrimPrefix(firstLine(raw), "ERROR:"))
		if reason == "" {
			reason = "(no reason given)"
		}
		return nil, nil, fmt.Errorf("implementer: declared ERROR: %s", reason)

	case strings.HasPrefix(raw, "diff --git"):
		return &Patch{
			Diff:         raw,
			FilesTouched: parseFilesFromDiff(raw),
		}, nil, nil

	default:
		// Format violation — the model wrote prose instead of one of
		// the three required prefixes. This is a common failure mode
		// when the model wants to request more files but phrases the
		// intent in natural language ("Looking at this, I need the
		// TSX call sites...") instead of `NEED_FILES: [...]`.
		//
		// Recover by re-sending with a specific correction that
		// quotes the broken response back and tells the model
		// exactly how to fix it. Bounded at maxFormatRetries so we
		// can't loop forever on a non-compliant model.
		if retries >= maxFormatRetries {
			return nil, nil, fmt.Errorf("implementer: expected 'diff --git', 'NEED_FILES:', or 'ERROR:' after %d format-correction attempts, got %q", retries, firstLine(raw))
		}
		corrected := userMsg +
			"\n\n---\n## YOUR PREVIOUS RESPONSE VIOLATED THE OUTPUT FORMAT\n\n" +
			"You wrote:\n\n> " + firstLine(raw) + "\n\n" +
			"This is not one of the three valid prefixes. You MUST respond with EXACTLY ONE of:\n\n" +
			"  diff --git        — if you can produce a complete unified diff now\n" +
			"  NEED_FILES: [...] — if you need to read more repo files first (JSON array of paths)\n" +
			"  ERROR: <reason>   — if the task is genuinely impossible\n\n" +
			"Reading your previous response, it looks like you wanted to request more files. Re-emit your response as a literal `NEED_FILES: [\"path/one.tsx\", \"path/two.tsx\"]` directive (a single line, JSON array of repo-relative paths). No prose. The harness will load those files and call you again with the expanded context.\n\n" +
			"If you do NOT need more files and can produce the diff directly, re-emit starting with `diff --git` on the first line and no prose preamble."
		return i.tryGenerateDiff(ctx, system, corrected, retries+1)
	}
}

// needFilesJSONRe matches the `[...]` JSON array that follows a
// `NEED_FILES:` marker line. Shared with parseFileSelection in spirit
// but kept separate so the grammar is clear: `NEED_FILES:` is a
// dedicated protocol, not a general-purpose JSON extractor.
var needFilesJSONRe = regexp.MustCompile(`(?s)\[.*\]`)

// parseNeedFiles extracts the repo-relative paths the Implementer
// asked for from a `NEED_FILES: [...]` response. Tolerates:
//
//   - extra whitespace before/after the JSON array
//   - a single trailing newline or prose after the array (we match the
//     first bracketed block only)
//
// Rejects malformed or unsafe paths the same way cleanFileList does.
// Exported indirectly via tests in implementer_test.go.
func parseNeedFiles(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "NEED_FILES:")
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("empty NEED_FILES directive")
	}
	match := needFilesJSONRe.FindString(raw)
	if match == "" {
		return nil, fmt.Errorf("no JSON array in NEED_FILES directive: %q", firstLine(raw))
	}
	var arr []string
	if err := json.Unmarshal([]byte(match), &arr); err != nil {
		return nil, fmt.Errorf("parse NEED_FILES JSON: %w", err)
	}
	return cleanFileList(arr), nil
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

// harvestPathRe matches what LOOKS like a repo-relative source file
// path: one or more path segments separated by forward slashes, ending
// in a known source-file extension. Leading drive letters, absolute
// paths, URL schemes, and trailing non-path characters are deliberately
// excluded by the pattern.
//
// We intentionally accept false positives here (strings that look like
// paths but aren't real files) — readFiles will reject any non-existent
// path via its os.Stat check and route it to the missing list. The
// cost of a false positive is one extra entry in the missing list; the
// cost of a false negative is the Implementer emitting ERROR when it
// should have loaded the file.
var harvestPathRe = regexp.MustCompile(`(?:^|[^A-Za-z0-9_\-./])([A-Za-z0-9_\-]+(?:/[A-Za-z0-9_\-.]+)+\.(?:tsx?|jsx?|mjs|cjs|go|py|rs|java|kt|rb|php|c|cc|cpp|h|hpp|swift|m|md|mdx|ya?ml|toml|json|yaml))\b`)

// harvestPaths extracts repo-relative source file paths that upstream
// agents (Scout brief, Critic report, Clarifier session, Architect
// sketch markdown) have cited in their prose. These paths are loaded
// directly into the Implementer's contents map at the start of Run()
// so the model never has to guess at paths that have already been
// verified by an earlier stage of the pipeline.
//
// The regex is deliberately permissive — false positives (strings
// that look like paths but aren't) are cheap because readFiles will
// reject them via os.Stat and report them in the missing list. False
// negatives (real paths the regex didn't match) are expensive because
// they force the Implementer to fall back to NEED_FILES or, in the
// worst case, emit ERROR claiming files don't exist.
//
// Capped at maxHarvestedPaths entries to keep the initial load
// bounded on prose-heavy inputs. De-duplicated. Filtered through
// isSafeRelPath.
func harvestPaths(inputs ...string) []string {
	const maxHarvestedPaths = 60
	seen := make(map[string]bool)
	var out []string
	for _, input := range inputs {
		if input == "" {
			continue
		}
		matches := harvestPathRe.FindAllStringSubmatch(input, -1)
		for _, m := range matches {
			p := strings.TrimSpace(m[1])
			// Strip a trailing `:` with a line-number annotation
			// (e.g. "foo/bar.tsx:193-199" → "foo/bar.tsx"). This
			// is how Clarifier evidence usually cites files.
			if idx := strings.Index(p, ":"); idx >= 0 {
				p = p[:idx]
			}
			if !isSafeRelPath(p) {
				continue
			}
			if seen[p] {
				continue
			}
			seen[p] = true
			out = append(out, p)
			if len(out) >= maxHarvestedPaths {
				return out
			}
		}
	}
	return out
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
