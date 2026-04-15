// `aidev config` subcommand tree (v0.5).
//
// Replaces the legacy `--preset` flag and per-file preset machinery
// with a single edit surface that both humans and Claude Code
// sessions can drive:
//
//	aidev config show              print resolved active profile
//	aidev config profile list      list all profiles in models.yaml
//	aidev config profile use NAME  switch active profile (with confirm)
//	aidev config set ROLE PROVIDER MODEL
//	                               tweak one role's tier in active profile
//	aidev config edit              open models.yaml in $EDITOR
//	aidev config doctor            validate the active profile
//
// Every mutation prompts for confirmation and prints a diff before
// touching the file. --yes bypasses the prompt for scripted callers
// (Claude Code's Bash tool, CI). The on-disk file's comments are
// preserved when possible by editing the YAML node tree in place
// rather than round-tripping through Marshal — see writeModelsFile
// below.
package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/aisu-ai/aidev/internal/config"
)

// runConfigSubcommand is the dispatcher for `aidev config <subsubcommand>`.
// Called from main.go's subcommand intercept block.
func runConfigSubcommand() {
	if len(os.Args) < 2 {
		configUsageAndExit()
	}
	sub := os.Args[1]
	// Trim os.Args so the inner subcommand sees a clean argv.
	os.Args = append(os.Args[:1], os.Args[2:]...)

	switch sub {
	case "show":
		runConfigShow()
	case "profile":
		runConfigProfile()
	case "set":
		runConfigSet()
	case "edit":
		runConfigEdit()
	case "doctor":
		runConfigDoctor()
	case "help", "-h", "--help":
		configUsageAndExit()
	default:
		fmt.Fprintf(os.Stderr, "aidev config: unknown subcommand %q\n\n", sub)
		configUsageAndExit()
	}
}

func configUsageAndExit() {
	fmt.Fprintln(os.Stderr, `Usage: aidev config <subcommand>

Subcommands:
  show                          Print the resolved active profile.
  profile list                  List all profiles defined in models.yaml.
  profile use <name>            Switch the active profile (with confirmation).
  set <role> <provider> <model> Update one role's tier in the active profile.
  edit                          Open models.yaml in $EDITOR.
  doctor                        Validate the active profile and routing.

All mutating commands prompt before writing. Pass --yes to skip the
prompt (for scripted callers and Claude Code sessions).`)
	os.Exit(2)
}

// ---------------------------------------------------------------------------
// aidev config show
// ---------------------------------------------------------------------------

func runConfigShow() {
	cfg := mustLoadConfig("")
	fmt.Printf("# aidev config (resolved)\n\n")
	fmt.Printf("config_dir:      %s\n", cfg.ConfigDir)
	fmt.Printf("active_profile:  %s\n", cfg.ActiveProfile)
	if cfg.RawModelsFile != nil {
		if p, ok := cfg.RawModelsFile.Profiles[cfg.ActiveProfile]; ok && strings.TrimSpace(p.Description) != "" {
			fmt.Printf("description:     %s\n", oneLine(p.Description))
		}
	}
	fmt.Println()
	fmt.Println("# Routing")
	roles := sortedKeys(cfg.Models.Routing)
	for _, role := range roles {
		tierName := cfg.Models.Routing[role]
		tier := cfg.Models.Tiers[tierName]
		modelDisplay := tier.Model
		if modelDisplay == "" {
			modelDisplay = "(default)"
		}
		fmt.Printf("  %-12s → %-12s  (%s:%s)\n", role, tierName, tier.Provider, modelDisplay)
	}
	fmt.Println()
	fmt.Println("# Tiers")
	tiers := sortedKeys(cfg.Models.Tiers)
	for _, name := range tiers {
		t := cfg.Models.Tiers[name]
		modelDisplay := t.Model
		if modelDisplay == "" {
			modelDisplay = "(default)"
		}
		fmt.Printf("  %-12s  provider=%s model=%s endpoint=%s max_tokens=%d temperature=%g\n",
			name, t.Provider, modelDisplay, t.Endpoint, t.MaxTokens, t.Temperature)
	}
}

// ---------------------------------------------------------------------------
// aidev config profile list / use
// ---------------------------------------------------------------------------

func runConfigProfile() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "aidev config profile: expected `list` or `use <name>`")
		os.Exit(2)
	}
	sub := os.Args[1]
	// Trim os.Args so the inner handler (runConfigProfileUse)
	// sees `[aidev, <profile-name>, ...]` rather than `[aidev,
	// use, <profile-name>, ...]`. Mirrors runConfigSubcommand.
	os.Args = append(os.Args[:1], os.Args[2:]...)
	switch sub {
	case "list":
		runConfigProfileList()
	case "use":
		runConfigProfileUse()
	default:
		fmt.Fprintf(os.Stderr, "aidev config profile: unknown subcommand %q\n", sub)
		os.Exit(2)
	}
}

func runConfigProfileList() {
	cfg := mustLoadConfig("")
	if cfg.RawModelsFile == nil {
		fatal("config show: RawModelsFile is nil — possible config corruption")
	}
	names := sortedProfileNames(cfg.RawModelsFile.Profiles)
	fmt.Printf("# Profiles in %s/models.yaml\n\n", cfg.ConfigDir)
	for _, name := range names {
		marker := "  "
		if name == cfg.ActiveProfile {
			marker = "* " // active marker
		}
		desc := strings.TrimSpace(cfg.RawModelsFile.Profiles[name].Description)
		desc = oneLine(desc)
		if desc == "" {
			desc = "(no description)"
		}
		fmt.Printf("%s%-12s  %s\n", marker, name, desc)
	}
	fmt.Println()
	fmt.Println("Switch with: aidev config profile use <name>")
}

func runConfigProfileUse() {
	yes := parseYesFlag()
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "aidev config profile use: missing profile name")
		os.Exit(2)
	}
	name := os.Args[1]

	cfg := mustLoadConfig("")
	if cfg.RawModelsFile == nil {
		fatal("config profile use: RawModelsFile is nil")
	}
	if _, ok := cfg.RawModelsFile.Profiles[name]; !ok {
		fmt.Fprintf(os.Stderr, "no such profile %q. Available:\n", name)
		for _, p := range sortedProfileNames(cfg.RawModelsFile.Profiles) {
			fmt.Fprintf(os.Stderr, "  %s\n", p)
		}
		os.Exit(2)
	}
	if name == cfg.ActiveProfile {
		fmt.Fprintf(os.Stderr, "aidev config profile use: %q is already the active profile\n", name)
		return
	}

	fmt.Fprintf(os.Stderr, "Switch active profile from %q to %q?\n", cfg.ActiveProfile, name)
	if !confirmOrYes(yes) {
		fmt.Fprintln(os.Stderr, "aborted")
		os.Exit(1)
	}

	if err := writeActiveProfile(cfg.ConfigDir, name); err != nil {
		fatal(fmt.Sprintf("config profile use: %v", err))
	}
	fmt.Fprintf(os.Stderr, "wrote: %s/models.yaml (active_profile=%s)\n", cfg.ConfigDir, name)
}

// ---------------------------------------------------------------------------
// aidev config set <role> <provider> <model>
// ---------------------------------------------------------------------------

func runConfigSet() {
	yes := parseYesFlag()
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "Usage: aidev config set <role> <provider> <model> [--yes]")
		os.Exit(2)
	}
	role, provider, model := os.Args[1], os.Args[2], os.Args[3]

	cfg := mustLoadConfig("")
	if cfg.RawModelsFile == nil {
		fatal("config set: RawModelsFile is nil")
	}
	profile, ok := cfg.RawModelsFile.Profiles[cfg.ActiveProfile]
	if !ok {
		fatal(fmt.Sprintf("config set: active profile %q not in profiles map", cfg.ActiveProfile))
	}
	currentTierName, ok := profile.Routing[role]
	if !ok {
		fmt.Fprintf(os.Stderr, "role %q is not routed in profile %q. Available roles:\n", role, cfg.ActiveProfile)
		for _, r := range sortedKeys(profile.Routing) {
			fmt.Fprintf(os.Stderr, "  %s\n", r)
		}
		os.Exit(2)
	}
	currentTier := profile.Tiers[currentTierName]

	fmt.Fprintf(os.Stderr, "Update %q tier in profile %q?\n", currentTierName, cfg.ActiveProfile)
	fmt.Fprintf(os.Stderr, "  before: provider=%s model=%s\n", currentTier.Provider, displayModel(currentTier.Model))
	fmt.Fprintf(os.Stderr, "  after:  provider=%s model=%s\n", provider, displayModel(model))
	fmt.Fprintf(os.Stderr, "(this changes the tier definition, so any other role pointing at %q is also affected)\n", currentTierName)
	if !confirmOrYes(yes) {
		fmt.Fprintln(os.Stderr, "aborted")
		os.Exit(1)
	}

	if err := writeTierProvider(cfg.ConfigDir, cfg.ActiveProfile, currentTierName, provider, model); err != nil {
		fatal(fmt.Sprintf("config set: %v", err))
	}
	fmt.Fprintf(os.Stderr, "wrote: %s/models.yaml (profile=%s tier=%s provider=%s model=%s)\n",
		cfg.ConfigDir, cfg.ActiveProfile, currentTierName, provider, displayModel(model))
}

// ---------------------------------------------------------------------------
// aidev config edit
// ---------------------------------------------------------------------------

func runConfigEdit() {
	cfg := mustLoadConfig("")
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = os.Getenv("VISUAL")
	}
	if editor == "" {
		editor = "vi"
	}
	path := filepath.Join(cfg.ConfigDir, "models.yaml")
	cmd := exec.Command(editor, path)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fatal(fmt.Sprintf("config edit: %v", err))
	}
	// Re-validate after the editor exits so the user catches a
	// typo immediately instead of on the next pipeline run.
	if _, err := config.Load(cfg.ConfigDir); err != nil {
		fmt.Fprintf(os.Stderr, "warning: post-edit validation failed: %v\n", err)
		fmt.Fprintln(os.Stderr, "your edit may have introduced a syntax or routing error. Run `aidev config doctor` to diagnose.")
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "config validates cleanly.")
}

// ---------------------------------------------------------------------------
// aidev config doctor
// ---------------------------------------------------------------------------

func runConfigDoctor() {
	cfg := mustLoadConfig("")
	failures := 0
	warnings := 0

	check := func(ok bool, severity, msg string) {
		switch {
		case ok:
			fmt.Fprintf(os.Stderr, "  ✓ %s\n", msg)
		case severity == "error":
			fmt.Fprintf(os.Stderr, "  ✗ %s\n", msg)
			failures++
		default:
			fmt.Fprintf(os.Stderr, "  ⚠ %s\n", msg)
			warnings++
		}
	}

	fmt.Fprintf(os.Stderr, "# aidev config doctor — profile=%s\n\n", cfg.ActiveProfile)

	// Basic file load already passed (we got cfg). Now apply
	// semantic checks.

	// Each tier must have a sane provider.
	validProviders := map[string]bool{
		"ollama":     true,
		"anthropic":  true,
		"claude-cli": true,
	}
	for _, name := range sortedKeys(cfg.Models.Tiers) {
		t := cfg.Models.Tiers[name]
		check(validProviders[t.Provider], "error",
			fmt.Sprintf("tier %q provider %q is recognised", name, t.Provider))
	}

	// Each routed role must point at a real tier.
	for _, role := range sortedKeys(cfg.Models.Routing) {
		_, ok := cfg.Models.Tiers[cfg.Models.Routing[role]]
		check(ok, "error",
			fmt.Sprintf("role %q routes to a defined tier", role))
	}

	// All known roles should be routed.
	requiredRoles := []string{
		"scout", "critic", "architect", "charter",
		"implementer", "reviewer", "tester", "coordinator",
	}
	for _, r := range requiredRoles {
		_, ok := cfg.Models.Routing[r]
		check(ok, "error",
			fmt.Sprintf("role %q is routed to a tier", r))
	}

	// Implementer/Reviewer on claude-cli is a quality warning,
	// not an error — they fall back to the legacy NEED_FILES path
	// instead of using v0.4 native tool use. Coordinator on
	// claude-cli is the recommended setup, so no warning there.
	for _, r := range []string{"implementer", "reviewer"} {
		tierName, ok := cfg.Models.Routing[r]
		if !ok {
			continue
		}
		tier := cfg.Models.Tiers[tierName]
		ok = tier.Provider != "claude-cli"
		check(ok, "warn",
			fmt.Sprintf("role %q uses a tool-capable provider (current: %s)", r, tier.Provider))
	}

	// Anthropic provider needs ANTHROPIC_API_KEY set somewhere.
	for _, name := range sortedKeys(cfg.Models.Tiers) {
		t := cfg.Models.Tiers[name]
		if t.Provider != "anthropic" {
			continue
		}
		ok := os.Getenv("ANTHROPIC_API_KEY") != ""
		check(ok, "error",
			fmt.Sprintf("tier %q (anthropic) requires ANTHROPIC_API_KEY", name))
	}

	// Tier without a model field — only valid for claude-cli
	// (which uses the user's Claude Code default).
	for _, name := range sortedKeys(cfg.Models.Tiers) {
		t := cfg.Models.Tiers[name]
		if t.Model == "" && t.Provider != "claude-cli" {
			check(false, "error",
				fmt.Sprintf("tier %q has empty model but provider %q requires one", name, t.Provider))
		}
	}

	// Tier timeout_seconds sanity check. Zero (the default) is
	// fine. < 60s is almost certainly a mistake — even a cloud
	// call can take 30+ seconds on a cold start. > 3600s (1 hour)
	// is suspiciously patient and likely means the user forgot
	// they set it; better to surface a warning.
	for _, name := range sortedKeys(cfg.Models.Tiers) {
		t := cfg.Models.Tiers[name]
		if t.TimeoutSeconds == 0 {
			continue
		}
		inRange := t.TimeoutSeconds >= 60 && t.TimeoutSeconds <= 3600
		check(inRange, "warn",
			fmt.Sprintf("tier %q timeout_seconds=%d is in the sane range (60-3600)", name, t.TimeoutSeconds))
	}

	fmt.Fprintln(os.Stderr)
	if failures > 0 {
		fmt.Fprintf(os.Stderr, "%d failure(s), %d warning(s) — fix the failures before running aidev.\n", failures, warnings)
		os.Exit(1)
	}
	if warnings > 0 {
		fmt.Fprintf(os.Stderr, "%d warning(s), 0 failures — config is usable but suboptimal.\n", warnings)
		return
	}
	fmt.Fprintln(os.Stderr, "all checks passed.")
}

// ---------------------------------------------------------------------------
// File mutation helpers — byte-level edits that preserve comments,
// blank lines, and the file's exact formatting.
//
// We deliberately do NOT use yaml.Marshal to round-trip the file
// because go-yaml v3 reformats whitespace (loses blank lines
// between top-level mapping entries, normalizes indentation) on
// save. The result still parses correctly but the user opens
// models.yaml after `aidev config profile use` and finds their
// hand-curated comments in a different shape than they left
// them. That's exactly the UX confusion v0.5 is trying to
// eliminate.
//
// Instead, we use yaml.Node parsing for one purpose only — to
// find the LINE NUMBER of the target field. Then we read the
// raw file bytes, replace the target line, and write the result.
// Comments, blank lines, and indentation are preserved by
// construction because we never touch any byte we don't have to.
// ---------------------------------------------------------------------------

// readModelsBytes reads models.yaml and returns its bytes.
func readModelsBytes(configDir string) ([]byte, string, error) {
	path := filepath.Join(configDir, "models.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("read %s: %w", path, err)
	}
	return data, path, nil
}

// writeModelsBytes saves new content to models.yaml, with a
// backup of the original at models.yaml.bak so the user (or our
// own validation rollback) can recover from a bad edit.
func writeModelsBytes(configDir string, original, updated []byte) error {
	path := filepath.Join(configDir, "models.yaml")
	backup := path + ".bak"
	if err := os.WriteFile(backup, original, 0o644); err != nil {
		return fmt.Errorf("write backup %s: %w", backup, err)
	}
	if err := os.WriteFile(path, updated, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// nodeLineForPath parses the YAML and returns the 1-indexed line
// number of the scalar value at the given key path. Returns 0
// and an error if the path doesn't resolve to a scalar.
//
// Path elements are mapping keys, e.g. ["active_profile"] or
// ["profiles", "default", "tiers", "small", "provider"].
func nodeLineForPath(data []byte, path []string) (int, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return 0, fmt.Errorf("parse: %w", err)
	}
	node := &root
	if node.Kind == yaml.DocumentNode {
		if len(node.Content) == 0 {
			return 0, fmt.Errorf("empty document")
		}
		node = node.Content[0]
	}
	for _, key := range path {
		if node.Kind != yaml.MappingNode {
			return 0, fmt.Errorf("expected mapping at key %q, got kind %d", key, node.Kind)
		}
		found := false
		for i := 0; i+1 < len(node.Content); i += 2 {
			if node.Content[i].Value == key {
				node = node.Content[i+1]
				found = true
				break
			}
		}
		if !found {
			return 0, fmt.Errorf("key %q not found at path %v", key, path)
		}
	}
	if node.Kind != yaml.ScalarNode {
		return 0, fmt.Errorf("path %v does not resolve to a scalar (kind %d)", path, node.Kind)
	}
	return node.Line, nil
}

// replaceScalarOnLine takes the file bytes, a 1-indexed line
// number, a YAML key (e.g. "provider"), and a new value, and
// returns the file with that line's value replaced. Preserves
// the original indentation by reading it from the existing line.
//
// The line is expected to look like:
//
//	  <indent><key>: <oldvalue>
//
// We split on the first ':', preserve everything up to and
// including the ':', and append " <newvalue>". The new value is
// quoted with go-yaml's standard scalar emitter (handles empty
// strings, special chars, etc.).
func replaceScalarOnLine(data []byte, line int, key, value string) ([]byte, error) {
	if line < 1 {
		return nil, fmt.Errorf("invalid line %d", line)
	}
	lines := strings.Split(string(data), "\n")
	if line > len(lines) {
		return nil, fmt.Errorf("line %d out of range (%d total)", line, len(lines))
	}
	idx := line - 1
	original := lines[idx]
	// Find the colon that separates key from value, accounting
	// for indentation. Stop at the first colon followed by a
	// space or end-of-line (to avoid colons inside quoted
	// strings — though aidev's keys are simple identifiers).
	colon := -1
	for i := 0; i < len(original); i++ {
		if original[i] != ':' {
			continue
		}
		if i+1 == len(original) || original[i+1] == ' ' || original[i+1] == '\t' {
			colon = i
			break
		}
	}
	if colon < 0 {
		return nil, fmt.Errorf("line %d has no key:value structure: %q", line, original)
	}
	// Sanity-check that the key on this line matches what the
	// caller expected. Catches a stale line number from a
	// previous file revision.
	prefix := strings.TrimSpace(original[:colon])
	if prefix != key {
		return nil, fmt.Errorf("line %d expected key %q, got %q (file may have changed since the line lookup)", line, key, prefix)
	}
	// Emit the new value via go-yaml so empty strings become
	// `""` and special characters get quoted. Trim the trailing
	// newline yaml.Marshal adds.
	emitted, err := yaml.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("marshal value: %w", err)
	}
	emittedStr := strings.TrimRight(string(emitted), "\n")
	lines[idx] = original[:colon+1] + " " + emittedStr
	return []byte(strings.Join(lines, "\n")), nil
}

// writeActiveProfile updates the top-level active_profile field
// in models.yaml. Comments, blank lines, indentation, and every
// other line in the file are preserved exactly. Validates the
// new file before saving.
func writeActiveProfile(configDir, name string) error {
	data, _, err := readModelsBytes(configDir)
	if err != nil {
		return err
	}
	line, err := nodeLineForPath(data, []string{"active_profile"})
	if err != nil {
		return fmt.Errorf("locate active_profile field: %w", err)
	}
	updated, err := replaceScalarOnLine(data, line, "active_profile", name)
	if err != nil {
		return fmt.Errorf("rewrite line %d: %w", line, err)
	}
	if err := writeModelsBytes(configDir, data, updated); err != nil {
		return err
	}
	if _, err := config.Load(configDir); err != nil {
		// Roll back from the backup so the user's file is left
		// in a valid state.
		bak := filepath.Join(configDir, "models.yaml.bak")
		main := filepath.Join(configDir, "models.yaml")
		_ = os.Rename(bak, main)
		return fmt.Errorf("post-write validation: %w (rolled back)", err)
	}
	return nil
}

// writeTierProvider updates one tier's provider AND model fields
// in the named profile. Other tier fields (endpoint, max_tokens,
// temperature) are preserved. Validates the result before saving.
//
// Implemented as two sequential single-line replacements:
// resolve the provider line, replace it; then resolve the model
// line in the now-mutated buffer, replace it.
func writeTierProvider(configDir, profileName, tierName, provider, model string) error {
	data, _, err := readModelsBytes(configDir)
	if err != nil {
		return err
	}

	providerPath := []string{"profiles", profileName, "tiers", tierName, "provider"}
	providerLine, err := nodeLineForPath(data, providerPath)
	if err != nil {
		return fmt.Errorf("locate %v: %w", providerPath, err)
	}
	updated, err := replaceScalarOnLine(data, providerLine, "provider", provider)
	if err != nil {
		return fmt.Errorf("rewrite provider on line %d: %w", providerLine, err)
	}

	modelPath := []string{"profiles", profileName, "tiers", tierName, "model"}
	modelLine, err := nodeLineForPath(updated, modelPath)
	if err != nil {
		return fmt.Errorf("locate %v: %w", modelPath, err)
	}
	updated, err = replaceScalarOnLine(updated, modelLine, "model", model)
	if err != nil {
		return fmt.Errorf("rewrite model on line %d: %w", modelLine, err)
	}

	if err := writeModelsBytes(configDir, data, updated); err != nil {
		return err
	}
	if _, err := config.Load(configDir); err != nil {
		bak := filepath.Join(configDir, "models.yaml.bak")
		main := filepath.Join(configDir, "models.yaml")
		_ = os.Rename(bak, main)
		return fmt.Errorf("post-write validation: %w (rolled back)", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// parseYesFlag scans os.Args for --yes and removes it. We don't
// use the flag package here because the config subcommands take
// positional arguments and mixing positional + flag parsing in
// the standard library is awkward.
func parseYesFlag() bool {
	yes := false
	out := os.Args[:1]
	for _, a := range os.Args[1:] {
		if a == "--yes" || a == "-y" {
			yes = true
			continue
		}
		out = append(out, a)
	}
	os.Args = out
	return yes
}

// confirmOrYes prompts on stderr and reads one line from stdin.
// Returns true on "y", "Y", "yes". Always returns true if yes is
// already set (--yes was passed).
func confirmOrYes(yes bool) bool {
	if yes {
		return true
	}
	fmt.Fprint(os.Stderr, "Apply? [y/N]: ")
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		return false
	}
	line = strings.TrimSpace(strings.ToLower(line))
	return line == "y" || line == "yes"
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedProfileNames(profiles map[string]config.Profile) []string {
	return sortedKeys(profiles)
}

func displayModel(m string) string {
	if m == "" {
		return "(default)"
	}
	return m
}

func oneLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return s[:i] + "..."
	}
	return s
}

// _ keeps time imported for potential future use in confirm prompts.
var _ = time.Now
