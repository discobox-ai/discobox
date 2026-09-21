package wellknown

import (
	"regexp"
	"testing"

	"github.com/discobox-ai/discobox/hostscope"
)

// Every entry is complete: a request expanded from one has to be a request the
// control plane accepts, and a malformed ID would never be asked for by name.
func TestEveryCredentialIsComplete(t *testing.T) {
	reverseDNS := regexp.MustCompile(`^[a-z0-9-]+(\.[a-z0-9-]+)+$`)
	seen := map[string]bool{}
	for _, c := range All() {
		if !reverseDNS.MatchString(c.ID) || seen[c.ID] {
			t.Errorf("%q is not a unique reverse-DNS ID", c.ID)
		}
		seen[c.ID] = true
		if c.Name == "" || c.Description == "" || c.EnvVar == "" || len(c.Hosts) == 0 {
			t.Errorf("%s is incomplete: %+v", c.ID, c)
		}
		for _, host := range c.Hosts {
			// An empty host is the wildcard scope, which nothing in this flow
			// may produce, and one written any other way than hostscope reads
			// it is a host the proxy's destination can never match — while
			// AllowsHost, which normalizes, would still say yes.
			if host == "" || host != hostscope.Normalize(host) {
				t.Errorf("%s names host %q, want a non-empty host as hostscope reads it", c.ID, host)
			}
		}
		if got, ok := Lookup(c.ID); !ok || got.ID != c.ID {
			t.Errorf("Lookup(%q) = %+v, %v", c.ID, got, ok)
		}
	}
	if c, ok := Lookup(GitHubAPI); !ok || c.Host() != "github.com" || c.EnvVar != "GH_TOKEN" {
		t.Fatalf("com.github.api is %+v", c)
	}
}

// A credential's host stands for the site: github.com is where repositories
// are pushed and pulled, and the API lives beneath it, so one entry answers
// for both. An ask may still name the narrower host it will reach.
func TestAHostCoversTheHostsBeneathIt(t *testing.T) {
	github, ok := Lookup(GitHubAPI)
	if !ok {
		t.Fatal("com.github.api is not registered")
	}
	for _, host := range []string{"github.com", "api.github.com", "uploads.github.com"} {
		if !github.AllowsHost(host) {
			t.Errorf("AllowsHost(%q) = false, want the site and the hosts beneath it", host)
		}
	}
	for _, host := range []string{"gitlab.com", "github.com.evil.example", "evil-github.com", ""} {
		if github.AllowsHost(host) {
			t.Errorf("AllowsHost(%q) = true, want only github.com and below", host)
		}
	}
}
