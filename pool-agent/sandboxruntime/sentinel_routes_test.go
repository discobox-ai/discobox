package sandboxruntime

import (
	"reflect"
	"testing"

	"github.com/discobox-ai/discobox/pkg/secretroute"
	poolagentapi "github.com/discobox-ai/discobox/pool-agent/api/gen"
	workerapimodel "github.com/discobox-ai/discobox/pool-agent/api/model"
)

// The route lists the control plane sends with a sentinel reach the proxy as
// lists, in order and each kept apart (ADR 26-10-27-880 §4): merging them
// would turn "every list allows" into "any route allows".
func TestProxySentinelRoutesKeepEachList(t *testing.T) {
	in := map[string][][]workerapimodel.SentinelRoute{
		"SENT": {
			{{Hosts: []string{"*"}}},
			{
				{Hosts: []string{"api.github.com"}, Paths: []string{"/repos/acme/**"}},
				{Hosts: []string{"api.github.com"}, Paths: []string{"/repos/acme/admin/**"}, Blocked: poolagentapi.NewOptBool(true)},
			},
		},
	}
	want := map[string][][]secretroute.Route{
		"SENT": {
			{{Hosts: []string{secretroute.AnyHost}}},
			{
				{Hosts: []string{"api.github.com"}, Paths: []string{"/repos/acme/**"}},
				{Hosts: []string{"api.github.com"}, Paths: []string{"/repos/acme/admin/**"}, Blocked: true},
			},
		},
	}
	if got := proxySentinelRoutes(in); !reflect.DeepEqual(got, want) {
		t.Fatalf("proxySentinelRoutes = %+v, want %+v", got, want)
	}
}

// Every part of a route the control plane sends reaches the proxy: a route
// that lost its methods, paths or protocol block would allow more than it
// says.
func TestSentinelRouteKeepsEveryPart(t *testing.T) {
	in := workerapimodel.SentinelRoute{
		Hosts:   []string{"api.github.com", "*.github.com"},
		Methods: []string{"POST"},
		Paths:   []string{"/graphql"},
		Graphql: poolagentapi.NewOptSentinelRouteGraphQL(poolagentapi.SentinelRouteGraphQL{
			Operations: []poolagentapi.SentinelRouteGraphQLOperationsItem{poolagentapi.SentinelRouteGraphQLOperationsItemQuery},
			Fields:     []string{"repository", "viewer*"},
		}),
		Blocked: poolagentapi.NewOptBool(true),
	}
	want := secretroute.Route{
		Hosts:   []string{"api.github.com", "*.github.com"},
		Methods: []string{"POST"},
		Paths:   []string{"/graphql"},
		GraphQL: &secretroute.GraphQL{Operations: []string{"query"}, Fields: []string{"repository", "viewer*"}},
		Blocked: true,
	}
	if got := sentinelRoute(in); !reflect.DeepEqual(got, want) {
		t.Fatalf("sentinelRoute = %+v, want %+v", got, want)
	}
	mcp := workerapimodel.SentinelRoute{
		Hosts: []string{"api.githubcopilot.com"},
		Mcp:   poolagentapi.NewOptSentinelRouteMCP(poolagentapi.SentinelRouteMCP{Tools: []string{"get_*"}}),
	}
	if got := sentinelRoute(mcp); !reflect.DeepEqual(got.MCP, &secretroute.MCP{Tools: []string{"get_*"}}) || got.GraphQL != nil {
		t.Fatalf("sentinelRoute(mcp) = %+v, want its tools and no GraphQL block", got)
	}
}
