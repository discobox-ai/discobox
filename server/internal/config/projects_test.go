package config

import (
	"reflect"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/proxy/gateway"
)

const projectID = "proj_6q3w8y5z7k9m4n6p"

// A project's gateways are written as the API's PUT body and come out as the
// routes the API would have stored (ADR 26-10-17-271 §1).
func TestFileNamesProjectGateways(t *testing.T) {
	clearConfigEnv(t)
	writeConfigFile(t, `
projects:
  default:
    gateways:
      anthropic:
        match: HTTPS://API.anthropic.com:443/v1/
        target: https://gateway.example.com/anthropic/v1
        requestHeaders:
          remove: [x-api-key]
          set:
            - name: cf-aig-authorization
              valueFrom: {env: DISCOBOX_GATEWAY_CF_TOKEN}
            - name: x-team
              value: platform
  `+projectID+`:
    gateways:
      openai:
        match: https://api.openai.com/v1
        target: https://gateway.example.com/openai/v1
`)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	defaultRoutes, byProject, err := cfg.GatewayRoutes()
	if err != nil {
		t.Fatalf("GatewayRoutes() error = %v", err)
	}
	wantDefault := []gateway.Route{{
		Name:   "anthropic",
		Match:  "https://api.anthropic.com/v1",
		Target: "https://gateway.example.com/anthropic/v1",
		RequestHeaders: gateway.HeaderModifier{
			Remove: []string{"X-Api-Key"},
			Set: []gateway.Header{
				{Name: "Cf-Aig-Authorization", Env: "DISCOBOX_GATEWAY_CF_TOKEN"},
				{Name: "X-Team", Value: "platform"},
			},
		},
	}}
	wantByProject := map[string][]gateway.Route{
		projectID: {{Name: "openai", Match: "https://api.openai.com/v1", Target: "https://gateway.example.com/openai/v1"}},
	}
	if !reflect.DeepEqual(defaultRoutes, wantDefault) || !reflect.DeepEqual(byProject, wantByProject) {
		t.Fatalf("GatewayRoutes() = %#v, %#v, want %#v, %#v", defaultRoutes, byProject, wantDefault, wantByProject)
	}
}

// What the API refuses, the file refuses, and the server does not start
// with an error naming where in the file it is (ADR 26-10-17-271 §1).
func TestFileRefusesAGatewayTheAPIWould(t *testing.T) {
	gateways := func(body string) string {
		return "projects:\n  default:\n    gateways:\n" + body
	}
	for _, tc := range []struct {
		name, body, want string
	}{
		{
			name: "a handle that is no project ID",
			body: "projects:\n  platform:\n    gateways: {}\n",
			want: "projects.platform",
		},
		{
			name: "an unknown key in a gateway",
			body: gateways("      a:\n        match: https://a.example\n        target: https://b.example\n        targett: https://c.example\n"),
			want: "targett",
		},
		{
			name: "a name that is no DNS label",
			body: gateways("      Anthropic:\n        match: https://a.example\n        target: https://b.example\n"),
			want: "projects.default.gateways.Anthropic",
		},
		{
			name: "two gateways with one match",
			body: gateways("      a:\n        match: https://a.example/v1\n        target: https://b.example\n      b:\n        match: https://a.example/v1/\n        target: https://c.example\n"),
			want: "already matches https://a.example/v1",
		},
		{
			name: "a header with a value and a variable",
			body: gateways("      a:\n        match: https://a.example\n        target: https://b.example\n        requestHeaders:\n          set: [{name: x-key, value: v, valueFrom: {env: DISCOBOX_GATEWAY_KEY}}]\n"),
			want: "exactly one of value and valueFrom",
		},
		{
			name: "a variable outside DISCOBOX_GATEWAY_",
			body: gateways("      a:\n        match: https://a.example\n        target: https://b.example\n        requestHeaders:\n          set: [{name: x-key, valueFrom: {env: DATABASE_DSN}}]\n"),
			want: "DISCOBOX_GATEWAY_",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearConfigEnv(t)
			writeConfigFile(t, tc.body)
			_, err := Load()
			if err == nil {
				t.Fatal("Load() accepted it")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load() error = %q, want it to name %q", err, tc.want)
			}
		})
	}
}
