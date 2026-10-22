package config

import (
	"fmt"
	"sort"
	"strings"

	"github.com/discobox-ai/discobox/proxy/gateway"
	"github.com/discobox-ai/x/id"
)

// defaultProjectHandle is the projects key that names the default user's
// default project (ADR 26-10-17-271 §2). Every other key is a project ID.
const defaultProjectHandle = "default"

// ProjectSettings is what the file keeps on one project (ADR 26-10-17-271).
type ProjectSettings struct {
	Gateways map[string]GatewaySettings `yaml:"gateways" doc:"Gateways the server keeps on the project, by name: a DNS label, unique in the project. Each is a PUT /projects/{id}/gateways/{name} body. The server creates or replaces each one at startup and refuses API changes to it; a gateway of the project the file does not name is left to the API."`
}

// GatewaySettings is one gateway, spelled as the API's PutProjectGatewayBody
// is (ADR 26-10-16-470 §1).
type GatewaySettings struct {
	Match          string                 `yaml:"match" doc:"The requests it routes, as an http(s) URL of an origin and a path prefix, matched on whole path segments; the longest match wins."`
	Target         string                 `yaml:"target" doc:"Where they go, as an http(s) URL of an origin and a path that replaces match's."`
	RequestHeaders GatewayHeaderModifiers `yaml:"requestHeaders" doc:"Headers removed, then set (replacing any value), then added (appending) on the way."`
}

// GatewayHeaderModifiers is the API's GatewayHeaderModifier.
type GatewayHeaderModifiers struct {
	Remove []string        `yaml:"remove" doc:"Header names removed."`
	Set    []GatewayHeader `yaml:"set" doc:"Headers set, replacing any value."`
	Add    []GatewayHeader `yaml:"add" doc:"Headers added, appending to any value."`
}

// GatewayHeader is the API's GatewayHeader: a name and exactly one of value
// and valueFrom.
type GatewayHeader struct {
	Name      string                   `yaml:"name" doc:"The header's name."`
	Value     string                   `yaml:"value" doc:"The header's value."`
	ValueFrom GatewayHeaderValueSource `yaml:"valueFrom" doc:"Where the header's value is read from when a request needs it."`
}

// GatewayHeaderValueSource is the API's GatewayHeaderValueSource.
type GatewayHeaderValueSource struct {
	Env string `yaml:"env" doc:"A variable of the server's environment, named DISCOBOX_GATEWAY_<NAME>, resolved when a request needs it and never stored. Only a gateway this file keeps may take a value from the environment."`
}

// GatewayRoutes is the file's gateways, normalized and sorted by name: those
// of the default entry, and those of each project named by ID. It refuses
// what the API would refuse, and a key that is neither default nor a project
// ID: a gateway that does not validate, and two gateways of one project with
// the same match (ADR 26-10-17-271 §1).
func (c *Config) GatewayRoutes() (defaultRoutes []gateway.Route, byProject map[string][]gateway.Route, err error) {
	byProject = map[string][]gateway.Route{}
	for handle, project := range c.Projects {
		if handle != defaultProjectHandle && (!strings.HasPrefix(handle, id.PrefixProject+"_") || !id.IsGenerated(handle)) {
			return nil, nil, fmt.Errorf("projects.%s: a project is named by %s or by its ID (%s_...)", handle, defaultProjectHandle, id.PrefixProject)
		}
		matches := map[string]string{}
		var routes []gateway.Route
		for name, settings := range project.Gateways {
			route, err := settings.route(name)
			if err != nil {
				return nil, nil, fmt.Errorf("projects.%s.gateways.%s: %w", handle, name, err)
			}
			if other, ok := matches[route.Match]; ok {
				return nil, nil, fmt.Errorf("projects.%s.gateways.%s: gateway %s already matches %s", handle, name, other, route.Match)
			}
			matches[route.Match] = name
			routes = append(routes, route)
		}
		sort.Slice(routes, func(i, j int) bool { return routes[i].Name < routes[j].Name })
		if handle == defaultProjectHandle {
			defaultRoutes = routes
		} else {
			byProject[handle] = routes
		}
	}
	return defaultRoutes, byProject, nil
}

func (g GatewaySettings) route(name string) (gateway.Route, error) {
	set, err := gatewayHeaders(g.RequestHeaders.Set)
	if err != nil {
		return gateway.Route{}, err
	}
	add, err := gatewayHeaders(g.RequestHeaders.Add)
	if err != nil {
		return gateway.Route{}, err
	}
	route := gateway.Normalize(gateway.Route{
		Name:           name,
		Match:          g.Match,
		Target:         g.Target,
		RequestHeaders: gateway.HeaderModifier{Remove: g.RequestHeaders.Remove, Set: set, Add: add},
	})
	return route, route.Validate()
}

func gatewayHeaders(in []GatewayHeader) ([]gateway.Header, error) {
	var out []gateway.Header
	for _, header := range in {
		if (header.Value != "") == (header.ValueFrom.Env != "") {
			return nil, fmt.Errorf("header %s needs exactly one of value and valueFrom", header.Name)
		}
		out = append(out, gateway.Header{Name: header.Name, Value: header.Value, Env: header.ValueFrom.Env})
	}
	return out, nil
}
