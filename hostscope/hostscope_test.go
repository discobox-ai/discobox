package hostscope_test

import (
	"testing"

	"github.com/discobox-ai/discobox/hostscope"
)

func TestCoversIsOneWay(t *testing.T) {
	for _, tc := range []struct {
		scope, host string
		want        bool
		why         string
	}{
		{"github.com", "github.com", true, "a scope covers itself"},
		{"github.com", "api.github.com", true, "a scope covers what is beneath it"},
		{"github.com", "uploads.github.com", true, "however deep the label"},
		{"github.com", "a.b.github.com", true, "at any depth"},
		{"api.github.com", "github.com", false, "a child is not authority over its parent"},
		{"api.github.com", "uploads.github.com", false, "siblings are different hosts"},
		{"github.com", "notgithub.com", false, "a suffix is not a subdomain"},
		{"github.com", "evilgithub.com", false, "nor is a longer label ending the same way"},
		{"", "api.github.com", true, "the wildcard covers everything"},
		{"github.com", "", false, "a destination nothing named is not covered"},
		{"API.GitHub.com", "api.github.com:443", true, "case and port are not what decides it"},
	} {
		if got := hostscope.Covers(tc.scope, tc.host); got != tc.want {
			t.Errorf("Covers(%q, %q) = %v, want %v: %s", tc.scope, tc.host, got, tc.want, tc.why)
		}
	}
}

func TestSpecificityPrefersTheNarrowerScope(t *testing.T) {
	exact := hostscope.Specificity("api.github.com", "api.github.com")
	parent := hostscope.Specificity("github.com", "api.github.com")
	wildcard := hostscope.Specificity("", "api.github.com")
	if exact >= parent || parent >= wildcard {
		t.Fatalf("ranks = %d, %d, %d; want the host itself, then a parent, then the wildcard", exact, parent, wildcard)
	}
}

func TestTooBroadCatchesASingleLabel(t *testing.T) {
	for _, scope := range []string{"com", "internal", "localhost"} {
		if !hostscope.TooBroad(scope) {
			t.Errorf("%q is not reported as too broad", scope)
		}
	}
	for _, scope := range []string{"", "github.com", "api.github.com"} {
		if hostscope.TooBroad(scope) {
			t.Errorf("%q is reported as too broad", scope)
		}
	}
}

func TestCommonParentNamesTheBindingThatWouldWork(t *testing.T) {
	for _, tc := range []struct{ a, b, want string }{
		{"api.github.com", "github.com", "github.com"},
		{"github.com", "api.github.com", "github.com"},
		{"api.github.com", "uploads.github.com", "github.com"},
		{"a.b.github.com", "c.github.com", "github.com"},
		{"api.github.com", "api.openai.com", ""},
		{"github.com", "example.com", ""},
		{"", "github.com", ""},
	} {
		if got := hostscope.CommonParent(tc.a, tc.b); got != tc.want {
			t.Errorf("CommonParent(%q, %q) = %q, want %q", tc.a, tc.b, got, tc.want)
		}
	}
}
