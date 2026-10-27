package secretenv

import "testing"

func TestName(t *testing.T) {
	for in, want := range map[string]string{
		"github":         "GITHUB",
		"npm-token":      "NPM_TOKEN",
		"GH_TOKEN":       "GH_TOKEN",
		"my.api  key":    "MY_API_KEY",
		"-leading-dash-": "LEADING_DASH",
		"1password":      "_1PASSWORD",
		"":               "",
	} {
		if got := Name(in); got != want {
			t.Errorf("Name(%q) = %q, want %q", in, got, want)
		}
	}
}
