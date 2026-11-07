package console

import "testing"

// An `issue=` or `pr=` tag on a discobox cut from a GitHub repository is the
// number of an issue or pull request there; any other tag, or one of those on
// a discobox with no GitHub repository to number it in, points nowhere.
func TestATagThatNumbersGitHubWorkLinksToIt(t *testing.T) {
	t.Parallel()
	box := Sandbox{Repository: "https://github.com/acme/foo"}
	for _, tc := range []struct{ tag, want string }{
		{"issue=4", "https://github.com/acme/foo/issues/4"},
		{"pr=123", "https://github.com/acme/foo/pull/123"},
		{"issue", ""},
		{"issue=", ""},
		{"issue=04", ""},
		{"issue=ENG-12", ""},
		{"pr=-1", ""},
		{"ticket=4", ""},
		{"wip", ""},
	} {
		if got := box.TagURL(tc.tag); got != tc.want {
			t.Errorf("tagURL(%q) = %q, want %q", tc.tag, got, tc.want)
		}
	}
	if got := (Sandbox{}).TagURL("issue=4"); got != "" {
		t.Errorf("tagURL on a discobox with no GitHub repository = %q, want nothing", got)
	}
}
