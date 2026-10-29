package apiroute

import (
	"reflect"
	"testing"

	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/pkg/secretroute"
)

// A route the CLI sends comes back from the API as the same route: every part
// survives the trip through the API's shape, so what a person typed is what
// is printed.
func TestARouteSurvivesTheAPI(t *testing.T) {
	for _, text := range []string{
		"*",
		"api.github.com,*.github.com",
		"GET,HEAD api.github.com/repos/*/*/pulls/**,/user",
		"api.github.com/graphql graphql=mutation:addComment,createPullRequest",
		"api.github.com/graphql graphql=*",
		"mcp.example.com/mcp mcp=search*,fetch",
		"mcp.example.com mcp=*=blocked",
		"api.github.com/repos/*/*/pulls/*/merge=blocked",
	} {
		route, err := secretroute.Parse(text)
		if err != nil {
			t.Fatalf("Parse(%q): %v", text, err)
		}
		input := Input(route)
		data, err := input.MarshalJSON()
		if err != nil {
			t.Fatalf("%q: marshal: %v", text, err)
		}
		var answered apimodel.SecretRoute
		if err := answered.UnmarshalJSON(data); err != nil {
			t.Fatalf("%q: unmarshal %s: %v", text, data, err)
		}
		if got := Route(answered); !reflect.DeepEqual(got, route) {
			t.Errorf("%q came back as %#v, want %#v", text, got, route)
		}
		if got := secretroute.Format(Route(answered)); got != secretroute.Format(route) {
			t.Errorf("%q is printed as %q", text, got)
		}
	}
}

// An empty list is sent as one, since that is what clears a list.
func TestNoRoutesIsAnEmptyList(t *testing.T) {
	if got := Inputs(nil); got == nil || len(got) != 0 {
		t.Fatalf("Inputs(nil) = %#v, want an empty list", got)
	}
}
