package model

import (
	"testing"

	"github.com/discobox-ai/discobox/internal/originkey"
)

// A sandbox is filed under its host and where its primary source came from —
// its directory, or its URL — and under its host alone with no source (ADR
// 0111). Nothing in the origin but the host goes into it.
func TestSandboxOriginKey(t *testing.T) {
	const host = "host_aaaaaaaaaaaaaaaa"
	origin := &Origin{HostID: host, Hostname: "laptop", User: "darren"}
	dir, url := "/src/alpha", "https://github.com/acme/api"

	for _, tc := range []struct {
		name   string
		origin *Origin
		source *GitSource
		want   string
	}{
		{"local source", origin, &GitSource{LocalDirectory: &dir}, originkey.Of(host, dir)},
		{"remote source", origin, &GitSource{URL: &url}, originkey.Of(host, url)},
		{"no source", origin, nil, originkey.Host(host)},
		{"no origin", nil, &GitSource{LocalDirectory: &dir}, ""},
		{"origin with no host", &Origin{}, &GitSource{LocalDirectory: &dir}, ""},
	} {
		if got := SandboxOriginKey(tc.origin, tc.source); got != tc.want {
			t.Errorf("%s: SandboxOriginKey = %q, want %q", tc.name, got, tc.want)
		}
	}
}
