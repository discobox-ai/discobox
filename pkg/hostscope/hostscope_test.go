package hostscope_test

import (
	"testing"

	"github.com/discobox-ai/discobox/pkg/hostscope"
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

func TestList(t *testing.T) {
	got := hostscope.List(" API.GitHub.com:443", "", "api.githubcopilot.com", "api.github.com")
	want := []string{"api.github.com", "api.githubcopilot.com"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("List = %q, want %q: normalized, deduplicated, in the order given", got, want)
	}
	if got := hostscope.List("", " "); got != nil {
		t.Fatalf("List of nothing = %q, want nil", got)
	}
}

func TestCoversAny(t *testing.T) {
	scopes := []string{"api.github.com", "githubcopilot.com"}
	for _, tc := range []struct {
		host string
		want bool
	}{
		{"api.github.com", true},
		{"api.githubcopilot.com", true},
		{"github.com", false},
		{"evil.example.com", false},
	} {
		if got := hostscope.CoversAny(scopes, tc.host); got != tc.want {
			t.Errorf("CoversAny(%q, %q) = %v, want %v", scopes, tc.host, got, tc.want)
		}
	}
	if !hostscope.CoversAny(nil, "anything.example.com") {
		t.Error("no scopes is the wildcard")
	}
	if scope, ok := hostscope.Covering(scopes, "api.individual.githubcopilot.com"); !ok || scope != "githubcopilot.com" {
		t.Errorf("Covering = %q, %v; want githubcopilot.com, true", scope, ok)
	}
	if _, ok := hostscope.Covering(scopes, "github.com"); ok {
		t.Error("Covering found a scope for a host none covers")
	}
}

func TestSpecificityAnyIsTheClosest(t *testing.T) {
	if got := hostscope.SpecificityAny([]string{"github.com", "api.github.com"}, "api.github.com"); got != 0 {
		t.Errorf("SpecificityAny = %d, want 0 from the exact host", got)
	}
	if got := hostscope.SpecificityAny(nil, "api.github.com"); got != 2 {
		t.Errorf("SpecificityAny(nil) = %d, want 2, the wildcard", got)
	}
}

func TestCoversEvery(t *testing.T) {
	scopes := []string{"github.com", "githubcopilot.com"}
	if !hostscope.CoversEvery(scopes, []string{"api.github.com", "api.githubcopilot.com"}) {
		t.Error("every host is beneath one of the scopes")
	}
	if hostscope.CoversEvery(scopes, []string{"api.github.com", "evil.example.com"}) {
		t.Error("one host outside the scopes is not covered")
	}
	if hostscope.CoversEvery(scopes, nil) {
		t.Error("the wildcard is covered only by the wildcard")
	}
	if !hostscope.CoversEvery(nil, nil) || !hostscope.CoversEvery(nil, []string{"anything.example.com"}) {
		t.Error("the wildcard covers everything")
	}
}
