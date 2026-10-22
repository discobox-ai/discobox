package imagecache

import "testing"

func TestParseReference(t *testing.T) {
	digest := "sha256:" + "ab0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcd"[:64]
	for _, tc := range []struct {
		in, name, host, repository string
	}{
		{"ghcr.io/discobox-ai/discobox-pool-agent:v1.2.3", "ghcr.io/discobox-ai/discobox-pool-agent:v1.2.3", "ghcr.io", "discobox-ai/discobox-pool-agent"},
		{"alpine:3.20", "docker.io/library/alpine:3.20", "registry-1.docker.io", "library/alpine"},
		{"user/tool:1", "docker.io/user/tool:1", "registry-1.docker.io", "user/tool"},
		{"localhost:5000/x:dev", "localhost:5000/x:dev", "localhost:5000", "x"},
		{"127.0.0.1:5000/a/b@" + digest, "127.0.0.1:5000/a/b@" + digest, "127.0.0.1:5000", "a/b"},
	} {
		ref, err := ParseReference(tc.in)
		if err != nil {
			t.Fatalf("%s: %v", tc.in, err)
		}
		if ref.Name() != tc.name || ref.apiHost() != tc.host || ref.Repository != tc.repository {
			t.Errorf("%s: got name %q host %q repository %q", tc.in, ref.Name(), ref.apiHost(), ref.Repository)
		}
	}
}

// A staged image has to be a particular image, so a reference that leaves the
// choice to whatever :latest is at the moment is refused, as is anything that
// could not be a repository path.
func TestParseReferenceRefuses(t *testing.T) {
	for _, in := range []string{"", "alpine", "ghcr.io/x/y", "ghcr.io/X/y:1", "ghcr.io/x/y@sha256:short", "ghcr.io/../y:1"} {
		if _, err := ParseReference(in); err == nil {
			t.Errorf("%q parsed", in)
		}
	}
}

func TestParseChallenge(t *testing.T) {
	scheme, params := parseChallenge(`Bearer realm="https://ghcr.io/token",service="ghcr.io",scope="repository:a/b:pull,push"`)
	if scheme != "Bearer" || params["realm"] != "https://ghcr.io/token" || params["service"] != "ghcr.io" || params["scope"] != "repository:a/b:pull,push" {
		t.Fatalf("got %q %v", scheme, params)
	}
}
