package agents

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// SketchRubric defines how the Selector scores Architect sketches.
// All weights are floats so a tweaked rubric can express "tension is
// half as bad as a violation" or "risks barely matter for trivial bug
// fixes". The shipped default lives in defaultRubricYAML below; users
// override per-repo with `<repo>/.aidev/sketch-rubric.yaml`.
type SketchRubric struct {
	// Version is a free-form string surfaced in the SelectorVerdict
	// so an audit trail can pin a decision to a specific rubric.
	// Bump when changing weights so old verdicts stay traceable.
	Version string `yaml:"version"`

	// Weights are the per-signal score contributions. Positives raise
	// a sketch's score, negatives lower it. RiskCap caps the total
	// risk penalty so a thoroughly-risk-aware sketch isn't punished
	// for honesty.
	Weights struct {
		Aligned   float64 `yaml:"aligned"`
		Tension   float64 `yaml:"tension"`
		Violation float64 `yaml:"violation"`
		Risk      float64 `yaml:"risk"`
		RiskCap   float64 `yaml:"risk_cap"`
	} `yaml:"weights"`

	// CriticalPrinciples is the allow-list for hard-veto: any sketch
	// whose Principle alignment marks one of these as "violation"
	// gets disqualified outright (score = -inf). Names are matched
	// case-insensitively against the principle column in the sketch's
	// `### Principle alignment` section.
	CriticalPrinciples []string `yaml:"critical_principles"`

	// IssueTypeOverrides tweaks scoring per issue label. Keys are
	// lower-cased label names ("bug", "feature", "enhancement"); when
	// the issue carries a matching label, the override's penalties
	// kick in. Falls back to "default" when no key matches.
	IssueTypeOverrides map[string]IssueTypeOverride `yaml:"issue_type_overrides"`

	// TieBreakerEpsilon is the score gap below which the Selector
	// calls the LLM tie-breaker instead of declaring a deterministic
	// winner. 0 means "always trust deterministic"; >0 means "if the
	// top two scores are within this distance, ask claude-cli."
	TieBreakerEpsilon float64 `yaml:"tie_breaker_epsilon"`

	// MinImplementableScore is the floor below which the Selector
	// refuses to pick (returns "needs refinement" instead). 0 means
	// "any positive sketch is implementable"; the default rubric sets
	// it to 1.0 so a sketch with more tension than alignment is
	// surfaced as needs-refinement rather than implemented.
	MinImplementableScore float64 `yaml:"min_implementable_score"`
}

// IssueTypeOverride is the per-issue-label scoring tweak. Currently
// only scope penalties; the structure leaves room for principle
// reweighting per type if we need it later.
type IssueTypeOverride struct {
	// ScopeFilesPenalty is subtracted from the score per file beyond
	// the issue-type's threshold. Set to 0 to disable.
	ScopeFilesPenalty float64 `yaml:"scope_files_penalty"`
	// ScopeFilesThreshold is the file count above which the penalty
	// kicks in. Files below this threshold cost nothing.
	ScopeFilesThreshold int `yaml:"scope_files_threshold"`
}

// defaultRubricYAML is the rubric that ships with aidev. Tuned for
// "make autonomous picks reproducible, fall through to LLM tie-break
// only when scores are genuinely close, and refuse to implement
// anything obviously bad."
//
// Rationale per knob:
//   - aligned/tension/violation form a 1/-1/-2 triangle so the math
//     is intuitive: "two alignments cancel one violation."
//   - risk weight is small (-0.5 capped at -3) because the Architect
//     is rewarded for surfacing risks; we don't want to penalise
//     thoroughness too hard.
//   - bug type penalises growing scope (a bug fix that touches 30
//     files is suspicious); feature type is neutral on scope.
//   - tie_breaker_epsilon=1.0 means a 0.99-point gap goes to the LLM,
//     a 1.01-point gap is decided deterministically.
//   - min_implementable_score=1.0 means at least net-positive
//     alignment is required; anything below triggers refinement.
//   - critical_principles list is short on purpose. Adding more here
//     makes the Selector less willing to pick anything; the user can
//     extend per-repo via .aidev/sketch-rubric.yaml.
const defaultRubricYAML = `
version: "1.0"
weights:
  aligned: 1.0
  tension: -1.0
  violation: -2.0
  risk: -0.5
  risk_cap: -3.0
critical_principles:
  - "Single Responsibility"
  - "DRY (Rule of Three)"
  - "Reversibility"
issue_type_overrides:
  bug:
    scope_files_penalty: 0.1
    scope_files_threshold: 5
  feature:
    scope_files_penalty: 0.0
    scope_files_threshold: 0
  default:
    scope_files_penalty: 0.05
    scope_files_threshold: 10
tie_breaker_epsilon: 1.0
min_implementable_score: 1.0
`

// LoadDefaultRubric parses the bundled default rubric. Errors here
// indicate a build problem (the YAML is broken in source) and should
// surface at startup, not at runtime — call this from agent
// construction, not the hot path.
func LoadDefaultRubric() (*SketchRubric, error) {
	return parseRubric([]byte(defaultRubricYAML))
}

// LoadRubricFile reads and parses a YAML rubric from disk, falling
// back to the default when path is empty or the file does not exist.
// A malformed YAML file is a hard error — better than silently
// reverting to the default and confusing the user about which weights
// are in effect.
func LoadRubricFile(path string) (*SketchRubric, error) {
	if strings.TrimSpace(path) == "" {
		return LoadDefaultRubric()
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return LoadDefaultRubric()
		}
		return nil, fmt.Errorf("rubric: read %s: %w", path, err)
	}
	r, err := parseRubric(data)
	if err != nil {
		return nil, fmt.Errorf("rubric: parse %s: %w", path, err)
	}
	return r, nil
}

func parseRubric(data []byte) (*SketchRubric, error) {
	var r SketchRubric
	if err := yaml.Unmarshal(data, &r); err != nil {
		return nil, err
	}
	if strings.TrimSpace(r.Version) == "" {
		return nil, errors.New("rubric: missing version")
	}
	return &r, nil
}

// IsCritical reports whether the named principle is in the rubric's
// critical-veto list. Comparison is case-insensitive and ignores
// surrounding whitespace so "DRY (Rule of Three)" matches whether the
// sketch capitalises the same way.
func (r *SketchRubric) IsCritical(principleName string) bool {
	needle := strings.ToLower(strings.TrimSpace(principleName))
	if needle == "" {
		return false
	}
	for _, p := range r.CriticalPrinciples {
		if strings.ToLower(strings.TrimSpace(p)) == needle {
			return true
		}
	}
	return false
}

// OverrideFor returns the issue-type tweak for the given labels (the
// labels slice is the GH issue's labels). Picks the FIRST label that
// has an override entry; falls back to the "default" entry; falls back
// to a zero-value override (no penalties).
func (r *SketchRubric) OverrideFor(labels []string) IssueTypeOverride {
	for _, label := range labels {
		key := strings.ToLower(strings.TrimSpace(label))
		if ov, ok := r.IssueTypeOverrides[key]; ok {
			return ov
		}
	}
	if ov, ok := r.IssueTypeOverrides["default"]; ok {
		return ov
	}
	return IssueTypeOverride{}
}
