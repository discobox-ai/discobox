// Package apiroute carries a secret route between the API's shape and
// secretroute.Route, the one reading of a route (ADR 26-10-27-880 §4). The CLI
// parses, formats and edits routes as secretroute.Route, so a route a person
// typed is sent, and a route the server answered is printed, through the same
// Parse and Format; this is the one place the two shapes meet.
package apiroute

import (
	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/pkg/secretroute"
)

// Input is a route as the API takes it.
func Input(route secretroute.Route) apimodel.SecretRouteInput {
	in := apimodel.SecretRouteInput{Hosts: route.Hosts, Methods: route.Methods, Paths: route.Paths}
	if route.GraphQL != nil {
		graphql := apimodel.SecretRouteGraphQL{Fields: route.GraphQL.Fields}
		for _, operation := range route.GraphQL.Operations {
			graphql.Operations = append(graphql.Operations, apiclientgen.SecretRouteGraphQLOperationsItem(operation))
		}
		in.Graphql = apiclientgen.NewOptSecretRouteGraphQL(graphql)
	}
	if route.MCP != nil {
		in.Mcp = apiclientgen.NewOptSecretRouteMCP(apimodel.SecretRouteMCP{Tools: route.MCP.Tools})
	}
	if route.Blocked {
		in.Blocked = apiclientgen.NewOptBool(true)
	}
	return in
}

// Inputs is routes as the API takes them. It is never nil, so an empty list
// is sent as one: what clears a list, where nil would leave it alone.
func Inputs(routes []secretroute.Route) []apimodel.SecretRouteInput {
	out := make([]apimodel.SecretRouteInput, 0, len(routes))
	for _, route := range routes {
		out = append(out, Input(route))
	}
	return out
}

// Route is a route the API answered with, as the CLI reads one. It is taken
// as written, not normalized: the server normalized it when it was stored,
// and what is printed is what is enforced.
func Route(route apimodel.SecretRoute) secretroute.Route {
	out := secretroute.Route{Hosts: route.Hosts, Methods: route.Methods, Paths: route.Paths, Blocked: route.Blocked.Or(false)}
	if graphql, ok := route.Graphql.Get(); ok {
		out.GraphQL = &secretroute.GraphQL{Fields: graphql.Fields}
		for _, operation := range graphql.Operations {
			out.GraphQL.Operations = append(out.GraphQL.Operations, string(operation))
		}
	}
	if mcp, ok := route.Mcp.Get(); ok {
		out.MCP = &secretroute.MCP{Tools: mcp.Tools}
	}
	return out
}

// Routes is a list of routes the API answered with.
func Routes(routes []apimodel.SecretRoute) []secretroute.Route {
	var out []secretroute.Route
	for _, route := range routes {
		out = append(out, Route(route))
	}
	return out
}
