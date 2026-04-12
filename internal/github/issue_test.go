package github

import "testing"

func TestParseURL(t *testing.T) {
	cases := []struct {
		in        string
		wantOwner string
		wantRepo  string
		wantNum   int
		wantErr   bool
	}{
		{"https://github.com/aisu-ai/ai-dev-standards/issues/42", "aisu-ai", "ai-dev-standards", 42, false},
		{"http://github.com/foo/bar/issues/1", "foo", "bar", 1, false},
		{"https://github.com/foo/bar/pull/1", "", "", 0, true},
		{"", "", "", 0, true},
		{"not a url", "", "", 0, true},
	}
	for _, c := range cases {
		o, r, n, err := ParseURL(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("ParseURL(%q) err=%v, wantErr=%v", c.in, err, c.wantErr)
			continue
		}
		if c.wantErr {
			continue
		}
		if o != c.wantOwner || r != c.wantRepo || n != c.wantNum {
			t.Errorf("ParseURL(%q) = %q, %q, %d; want %q, %q, %d", c.in, o, r, n, c.wantOwner, c.wantRepo, c.wantNum)
		}
	}
}
