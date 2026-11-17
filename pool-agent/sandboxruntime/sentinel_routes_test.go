package sandboxruntime

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/go-faster/jx"

	"github.com/discobox-ai/discobox/pkg/secretroute"
	poolagentapi "github.com/discobox-ai/discobox/pool-agent/api/gen"
	workerapimodel "github.com/discobox-ai/discobox/pool-agent/api/model"
)

// The route lists a control plane from before grants sends with a sentinel
// reach the proxy as one grant with no expiry, its lists in order and each
// kept apart (ADR 26-10-27-880 §4): merging them would turn "every list
// allows" into "any route allows".
func TestProxySentinelRoutesAreOneGrantKeepingEachList(t *testing.T) {
	in := map[string][][]workerapimodel.SentinelRoute{
		"SENT": {
			{{Hosts: []string{"*"}}},
			{
				{Hosts: []string{"api.github.com"}, Paths: []string{"/repos/acme/**"}},
				{Hosts: []string{"api.github.com"}, Paths: []string{"/repos/acme/admin/**"}, Blocked: poolagentapi.NewOptBool(true)},
			},
		},
	}
	want := map[string][]secretroute.Grant{
		"SENT": {{Lists: [][]secretroute.Route{
			{{Hosts: []string{secretroute.AnyHost}}},
			{
				{Hosts: []string{"api.github.com"}, Paths: []string{"/repos/acme/**"}},
				{Hosts: []string{"api.github.com"}, Paths: []string{"/repos/acme/admin/**"}, Blocked: true},
			},
		}}},
	}
	if got := proxySentinelGrants(nil, in); !reflect.DeepEqual(got, want) {
		t.Fatalf("proxySentinelGrants = %+v, want %+v", got, want)
	}
}

// Grants reach the proxy each apart, in order, with their expiry, and in
// place of any route lists beside them (ADR 26-11-03-329 §3).
func TestProxySentinelGrantsKeepEachGrantAndItsExpiry(t *testing.T) {
	expires := time.Date(2026, 11, 3, 13, 0, 0, 0, time.UTC)
	grants := map[string][]workerapimodel.SentinelGrant{
		"SENT": {
			{Lists: [][]workerapimodel.SentinelRoute{{{Hosts: []string{"api.github.com"}, Methods: []string{"GET"}}}}},
			{Lists: [][]workerapimodel.SentinelRoute{{{Hosts: []string{"api.github.com"}, Methods: []string{"POST"}}}}, ExpiresAt: poolagentapi.NewOptDateTime(expires)},
		},
	}
	routes := map[string][][]workerapimodel.SentinelRoute{"SENT": {{{Hosts: []string{"*"}}}}}
	want := map[string][]secretroute.Grant{
		"SENT": {
			{Lists: [][]secretroute.Route{{{Hosts: []string{"api.github.com"}, Methods: []string{"GET"}}}}},
			{Lists: [][]secretroute.Route{{{Hosts: []string{"api.github.com"}, Methods: []string{"POST"}}}}, ExpiresAt: &expires},
		},
	}
	if got := proxySentinelGrants(grants, routes); !reflect.DeepEqual(got, want) {
		t.Fatalf("proxySentinelGrants = %+v, want %+v", got, want)
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
	ws := workerapimodel.SentinelRoute{
		Hosts: []string{"gateway.discord.gg"},
		Websocket: poolagentapi.NewOptSentinelRouteWebSocket(poolagentapi.SentinelRouteWebSocket{Places: []poolagentapi.SentinelRouteMessagePlace{
			{At: "/d/token", When: poolagentapi.NewOptSentinelRouteMessagePlaceWhen(poolagentapi.SentinelRouteMessagePlaceWhen{"op": jx.Raw("2")})},
			{At: "/auth"},
		}}),
	}
	wantWS := &secretroute.WebSocket{Places: []secretroute.MessagePlace{
		{At: "/d/token", When: map[string]json.RawMessage{"op": json.RawMessage("2")}},
		{At: "/auth"},
	}}
	if got := sentinelRoute(ws); !reflect.DeepEqual(got.WebSocket, wantWS) {
		t.Fatalf("sentinelRoute(ws) = %+v, want its message places", got.WebSocket)
	}
}
