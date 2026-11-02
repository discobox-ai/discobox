package requestrole

import (
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/pkg/secretroute"
)

func route(t *testing.T, text string) secretroute.Route {
	t.Helper()
	r, err := secretroute.Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// Roles group by document, never by name: two documents a discobox named
// alike are both shown, and marked as different.
func TestGroupIsByDocumentNotName(t *testing.T) {
	roles := Group([]Policy{
		{Name: "github-submit", Digest: "aaa", Args: map[string]string{"repo": "web"}, Routes: []secretroute.Route{route(t, "POST api.github.com/repos/acme/web/pulls")}},
		{Name: "github-submit", Digest: "aaa", Args: map[string]string{"repo": "api"}, Routes: []secretroute.Route{route(t, "POST api.github.com/repos/acme/api/pulls")}},
		{Name: "github-submit", Digest: "bbb", Args: map[string]string{"repo": "vm"}, UnusedArgs: []string{"repo"}, Routes: []secretroute.Route{route(t, "POST api.github.com/**")}},
	})
	if len(roles) != 2 {
		t.Fatalf("roles = %+v, want two documents", roles)
	}
	if roles[0].ArgsText() != "repo=web; repo=api" || !roles[0].NameShared || !roles[1].NameShared {
		t.Fatalf("roles = %+v", roles)
	}
	notes := strings.Join(roles[1].Notes(), "\n")
	if !strings.Contains(notes, "different document") || !strings.Contains(notes, "args not used by any route: repo") {
		t.Fatalf("notes = %q", notes)
	}
}

// The summary says the path scope, wildcards marked: a role labeled "one
// repository" that grants all of api.github.com does not read like one that
// does not.
func TestSummarySaysThePathScope(t *testing.T) {
	wide := Summary([]secretroute.Route{route(t, "POST api.github.com/**")})
	narrow := Summary([]secretroute.Route{route(t, "POST api.github.com/repos/acme/vm/pulls")})
	if wide == narrow || !strings.Contains(wide, "/[**]") || !strings.Contains(narrow, "/repos/acme/vm/pulls") {
		t.Fatalf("wide %q, narrow %q", wide, narrow)
	}
	got := Summary([]secretroute.Route{
		route(t, "api.github.com/graphql graphql=query"),
		route(t, "github.com/acme/*/info=blocked"),
	})
	for _, want := range []string{"api.github.com", "any method", "/graphql", "graphql query", "1 blocked"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary %q lacks %q", got, want)
		}
	}
	if got := Summary([]secretroute.Route{route(t, "GET example.com")}); !strings.Contains(got, "any path") {
		t.Errorf("summary %q, want any path", got)
	}
}
