package services

import (
	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/pkg/secretroute"
	"github.com/discobox-ai/discobox/server/internal/model"
)

// The API's secret route types and the model's (secretroute.Route), converted
// in one place. Nothing here normalizes: a route is normalized where it is
// written (model.NormalizeRoutes), which refuses one that is malformed.

// SecretRoutesToModel reads the routes a request body names.
func SecretRoutesToModel(in []serverapi.SecretRouteInput) []model.SecretRoute {
	out := make([]model.SecretRoute, 0, len(in))
	for _, route := range in {
		next := model.SecretRoute{Hosts: route.Hosts, Methods: route.Methods, Paths: route.Paths, Blocked: route.Blocked.Or(false)}
		if block, ok := route.Graphql.Get(); ok {
			next.GraphQL = &secretroute.GraphQL{Fields: block.Fields}
			for _, operation := range block.Operations {
				next.GraphQL.Operations = append(next.GraphQL.Operations, string(operation))
			}
		}
		if block, ok := route.Mcp.Get(); ok {
			next.MCP = &secretroute.MCP{Tools: block.Tools}
		}
		out = append(out, next)
	}
	return out
}

// SecretRoutesFromModel is a route list as the API serves it.
func SecretRoutesFromModel(in []model.SecretRoute) []serverapi.SecretRoute {
	out := make([]serverapi.SecretRoute, 0, len(in))
	for _, route := range in {
		api := serverapi.SecretRoute{Hosts: route.Hosts, Methods: route.Methods, Paths: route.Paths}
		if api.Hosts == nil {
			api.Hosts = []string{}
		}
		if route.GraphQL != nil {
			block := serverapi.SecretRouteGraphQL{Fields: route.GraphQL.Fields}
			for _, operation := range route.GraphQL.Operations {
				block.Operations = append(block.Operations, serverapi.SecretRouteGraphQLOperationsItem(operation))
			}
			api.Graphql = serverapi.NewOptSecretRouteGraphQL(block)
		}
		if route.MCP != nil {
			api.Mcp = serverapi.NewOptSecretRouteMCP(serverapi.SecretRouteMCP{Tools: route.MCP.Tools})
		}
		if route.Blocked {
			api.Blocked = serverapi.NewOptBool(true)
		}
		out = append(out, api)
	}
	return out
}
