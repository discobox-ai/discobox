package harness

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/discobox-ai/discobox/pkg/harnesscapture"
)

// A secret an image declares reads its routes in the shape routes have now,
// and one an older image declares as it was meant: a host and every host
// beneath it, and a route still declared with a use, which was judged, as
// blocked, never as an open route (ADR 26-10-27-880 §8).
func TestADeclaredSecretReadsItsRoutes(t *testing.T) {
	for _, tc := range []struct {
		declared string
		want     []harnesscapture.Route
	}{
		{`[{"hosts":["githubcopilot.com"],"paths":["/mcp/**"],"blocked":true}]`,
			[]harnesscapture.Route{{Hosts: []string{"githubcopilot.com"}, Paths: []string{"/mcp/**"}, Blocked: true}}},
		{`[{"host":"githubcopilot.com","path":"/mcp","use":"read-only queries"}, {"host":"github.com","use":"  "}]`,
			[]harnesscapture.Route{
				{Hosts: []string{"*.githubcopilot.com", "githubcopilot.com"}, Paths: []string{"/mcp/**"}, Blocked: true},
				{Hosts: []string{"*.github.com", "github.com"}},
			}},
	} {
		var got Secret
		if err := json.Unmarshal([]byte(`{"name":"TOKEN","routes":`+tc.declared+`}`), &got); err != nil {
			t.Fatalf("%s: %v", tc.declared, err)
		}
		if !reflect.DeepEqual(got.Routes, tc.want) {
			t.Fatalf("%s = %+v, want %+v", tc.declared, got.Routes, tc.want)
		}
	}
}

// A missing static declaration must not hide a captured provider. Every
// credential type needs registered policy or an explicit refusal to capture.
func TestBuiltInCredentialDestinationInventory(t *testing.T) {
	builtins, err := Builtins()
	if err != nil {
		t.Fatal(err)
	}
	for _, declaration := range builtins {
		path := declaration.Directory
		var secrets []Secret
		if len(declaration.Credentials.Secrets) > 0 {
			if err := json.Unmarshal(declaration.Credentials.Secrets, &secrets); err != nil {
				t.Fatal(err)
			}
		}
		if path == "shell" {
			if len(secrets) > 0 || declaration.SaveBack() != nil {
				t.Fatal("shell now collects credentials: add explicit policy")
			}
			continue
		}
		d := declaration.SaveBack()
		if d == nil || len(d.Providers) == 0 {
			t.Fatalf("%s: coding harness has no credential inventory", path)
		}
		for _, p := range d.Providers {
			for _, kind := range p.Types {
				if len(p.CredentialRoutes(kind)) == 0 && p.ManualRoutes == "" {
					t.Errorf("%s: %s/%s has no destination policy", path, p.ID, kind)
				}
				for _, r := range p.CredentialRoutes(kind) {
					for _, h := range r.Hosts {
						if h == "*" {
							t.Errorf("%s: %s implicitly unrestricted", path, p.ID)
						}
					}
				}
			}
		}
		for _, s := range secrets {
			if len(s.Routes) == 0 {
				t.Errorf("%s: static secret %s has no routes", path, s.Name)
			}
			found := false
			for _, p := range d.Providers {
				if p.EnvName == s.Name {
					found = true
					for _, kind := range p.Types {
						if !reflect.DeepEqual(s.Routes, p.CredentialRoutes(kind)) {
							t.Errorf("%s: static/provider policy differs for %s", path, s.Name)
						}
					}
				}
			}
			if !found {
				t.Errorf("%s: secret %s has no capture policy", path, s.Name)
			}
		}
	}
}
