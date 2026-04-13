package agents

import (
	"errors"
	"os"
	"regexp"
	"strings"
)

// ParseFollowUpsFile parses a `.aidev/followups.md` file previously
// written by the Reviewer. The file format is produced by
// Review.WriteFollowUps — each follow-up is introduced by a
// "## N. Title" heading and terminated by a "---" horizontal rule,
// with an optional "labels: a, b" line right after the heading.
//
// Exported so `aidev followups --file-issues` and any future caller
// can consume the file without hand-rolling a parser.
func ParseFollowUpsFile(path string) ([]FollowUpIssue, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseFollowUpsMarkdown(string(data)), nil
}

// followUpHeadingRe matches a "## N. Title" line.
var followUpHeadingRe = regexp.MustCompile(`^## \d+\.\s+(.+)$`)

// followUpLabelsRe matches a "labels: a, b, c" line (lower-cased prefix).
var followUpLabelsRe = regexp.MustCompile(`^labels:\s*(.+)$`)

// parseFollowUpsMarkdown is the internal tokeniser. Split on the
// horizontal rules (`---`), then decode each section's heading, labels
// line, and body. Sections that don't start with a numbered heading
// are silently skipped — the top matter (file header + timestamp) is
// expected to not match and should be discarded naturally.
func parseFollowUpsMarkdown(md string) []FollowUpIssue {
	// Split into sections at horizontal rules.
	sections := splitOnHorizontalRule(md)

	var out []FollowUpIssue
	for _, s := range sections {
		fu := parseFollowUpSection(s)
		if fu.Title == "" {
			continue
		}
		out = append(out, fu)
	}
	return out
}

// splitOnHorizontalRule tokenises the markdown into sections delimited
// by "---" lines at the left margin. Consecutive blank lines around the
// rule are preserved inside the sections (they're trimmed later).
func splitOnHorizontalRule(md string) []string {
	var sections []string
	var current strings.Builder
	for _, line := range strings.Split(md, "\n") {
		if strings.TrimSpace(line) == "---" {
			if current.Len() > 0 {
				sections = append(sections, current.String())
				current.Reset()
			}
			continue
		}
		current.WriteString(line)
		current.WriteString("\n")
	}
	if current.Len() > 0 {
		sections = append(sections, current.String())
	}
	return sections
}

// parseFollowUpSection extracts a single FollowUpIssue from one
// horizontal-rule-delimited section. Returns a zero value if the
// section has no "## N. Title" heading.
func parseFollowUpSection(section string) FollowUpIssue {
	lines := strings.Split(section, "\n")
	var fu FollowUpIssue
	var bodyLines []string
	seenHeading := false
	for _, line := range lines {
		if !seenHeading {
			if m := followUpHeadingRe.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
				fu.Title = strings.TrimSpace(m[1])
				seenHeading = true
				continue
			}
			continue
		}
		// Post-heading: check for labels line before body starts.
		trimmed := strings.TrimSpace(line)
		if fu.Labels == nil && strings.HasPrefix(trimmed, "labels:") {
			if m := followUpLabelsRe.FindStringSubmatch(trimmed); m != nil {
				for _, l := range strings.Split(m[1], ",") {
					l = strings.TrimSpace(l)
					if l != "" {
						fu.Labels = append(fu.Labels, l)
					}
				}
			}
			continue
		}
		bodyLines = append(bodyLines, line)
	}
	fu.Body = strings.TrimSpace(strings.Join(bodyLines, "\n"))
	return fu
}

// FollowUpFiler is a callback used by FileFollowUps so the subcommand
// can inject a real GitHub client (or a mock for tests) without this
// package importing github directly.
type FollowUpFiler func(title, body string, labels []string) (number int, url string, err error)

// FileFollowUps iterates the given proposals and calls the filer for
// each one. It returns the list of (number, URL) pairs for issues it
// successfully filed, and the first error encountered (filing is
// interrupted at the first error — we don't want to leave a partial
// mess on the repo without the user knowing).
func FileFollowUps(proposals []FollowUpIssue, file FollowUpFiler) ([]FollowUpFiled, error) {
	if file == nil {
		return nil, errors.New("followups: nil filer")
	}
	out := make([]FollowUpFiled, 0, len(proposals))
	for _, p := range proposals {
		num, url, err := file(p.Title, p.Body, p.Labels)
		if err != nil {
			return out, err
		}
		out = append(out, FollowUpFiled{Proposal: p, Number: num, URL: url})
	}
	return out, nil
}

// FollowUpFiled is the result of filing a single proposal as an issue.
type FollowUpFiled struct {
	Proposal FollowUpIssue
	Number   int
	URL      string
}
