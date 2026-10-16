package egress

import "testing"

func TestMatchDomain(t *testing.T) {
	cases := []struct {
		pattern, host string
		want          bool
	}{
		{"*", "anything.example.com", true},
		{"github.com", "GitHub.com", true},
		{"github.com", "api.github.com", false},
		{"*.github.com", "api.github.com", true},
		{"*.github.com", "github.com", false},
		{"registry.*", "registry.npmjs.org", true},
		{"registry.*", "registryx.npmjs.org", false},
	}
	for _, tc := range cases {
		if got := matchDomain(tc.pattern, tc.host); got != tc.want {
			t.Errorf("matchDomain(%q, %q) = %v, want %v", tc.pattern, tc.host, got, tc.want)
		}
	}
}
