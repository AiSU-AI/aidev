package agents

import "testing"

func TestExtractRecommendation(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"explicit build", "long report\n\nRECOMMENDATION: build\n", "build"},
		{"defer mixed case", "body\nRecommendation: Defer\n", "defer"},
		{"kill with trailing spaces", "body\nRECOMMENDATION:   kill  ", "kill"},
		{"unclear preserved", "body\nRECOMMENDATION: unclear", "unclear"},
		{"missing defaults unclear", "just some body without a verdict", "unclear"},
		{"unknown verb defaults unclear", "body\nRECOMMENDATION: maybe", "unclear"},
	}
	for _, c := range cases {
		got := extractRecommendation(c.in)
		if got != c.want {
			t.Errorf("%s: extractRecommendation = %q, want %q", c.name, got, c.want)
		}
	}
}
