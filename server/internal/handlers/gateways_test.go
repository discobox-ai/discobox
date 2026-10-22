package handlers

import (
	"context"
	"net/http"
	"testing"
	"time"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/pool-agent/poolauth"
	"github.com/discobox-ai/discobox/proxy/gateway"
	"github.com/discobox-ai/discobox/server/internal/auth"
	"github.com/discobox-ai/discobox/server/internal/model"
	svcapi "github.com/discobox-ai/discobox/server/internal/services"
)

// fakeGatewayService stores what it is put and answers resolves from env.
type fakeGatewayService struct {
	put      gateway.Route
	resolved []string
}

func (f *fakeGatewayService) ListProjectGateways(context.Context, string) ([]model.ProjectGateway, error) {
	return nil, nil
}

func (f *fakeGatewayService) GetProjectGateway(_ context.Context, projectID, name string) (*model.ProjectGateway, error) {
	return &model.ProjectGateway{
		ProjectID: projectID, Name: name, ManagedBy: model.GatewayManagedByConfig,
		RequestHeaders: gateway.HeaderModifier{Set: []gateway.Header{{Name: "Cf-Aig-Authorization", Env: "DISCOBOX_GATEWAY_KEY"}}},
	}, nil
}

func (f *fakeGatewayService) PutProjectGateway(_ context.Context, projectID, name string, route gateway.Route) (*model.ProjectGateway, error) {
	f.put = route
	return &model.ProjectGateway{
		ProjectID: projectID, Name: name, Match: route.Match, Target: route.Target,
		RequestHeaders: route.RequestHeaders, ManagedBy: model.GatewayManagedByAPI, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}, nil
}

func (f *fakeGatewayService) DeleteProjectGateway(context.Context, string, string) error { return nil }

func (f *fakeGatewayService) ResolveGatewayHeaders(_ context.Context, _, _, _ string, env []string) (map[string]string, error) {
	f.resolved = env
	return map[string]string{"DISCOBOX_GATEWAY_KEY": "gw-key"}, nil
}

// A put's headers are written values, and read back as written.
func TestPutProjectGatewayWritesValues(t *testing.T) {
	svc := &fakeGatewayService{}
	h := New(svcapi.Services{Gateways: svc})
	res, err := h.PutProjectGateway(context.Background(), &serverapi.PutProjectGatewayBody{
		Match:  "https://api.anthropic.com/v1",
		Target: "https://gw.example.com/anthropic/v1",
		RequestHeaders: serverapi.NewOptPutGatewayHeaderModifier(serverapi.PutGatewayHeaderModifier{
			Remove: []string{"X-Api-Key"},
			Set:    []serverapi.PutGatewayHeader{{Name: "X-Team", Value: "platform"}},
		}),
	}, serverapi.PutProjectGatewayParams{ProjectId: "project-1", GatewayName: "anthropic"})
	if err != nil {
		t.Fatal(err)
	}
	set := svc.put.RequestHeaders.Set
	if len(set) != 1 || set[0].Value != "platform" || set[0].Env != "" {
		t.Fatalf("put %+v", svc.put)
	}
	body, ok := res.(*serverapi.ProjectGateway)
	if !ok {
		t.Fatalf("response = %T", res)
	}
	if got := body.RequestHeaders.Set; got[0].Value.Or("") != "platform" || got[0].ValueFrom.IsSet() {
		t.Fatalf("read back %+v", got)
	}
	if body.ManagedBy != serverapi.ProjectGatewayManagedByAPI {
		t.Fatalf("managedBy = %q, want api", body.ManagedBy)
	}
}

// A gateway the server configuration file keeps says so, and a header it
// takes from the server's environment reads back as the variable's name,
// never its value (ADR 26-10-17-271 §3).
func TestGetProjectGatewaySaysTheFileKeepsIt(t *testing.T) {
	h := New(svcapi.Services{Gateways: &fakeGatewayService{}})
	res, err := h.GetProjectGateway(context.Background(), serverapi.GetProjectGatewayParams{ProjectId: "project-1", GatewayName: "anthropic"})
	if err != nil {
		t.Fatal(err)
	}
	body, ok := res.(*serverapi.ProjectGateway)
	if !ok || body.ManagedBy != serverapi.ProjectGatewayManagedByConfig {
		t.Fatalf("response = %#v, want managedBy config", res)
	}
	if got := body.RequestHeaders.Set; len(got) != 1 || got[0].ValueFrom.Or(serverapi.GatewayHeaderValueSource{}).Env != "DISCOBOX_GATEWAY_KEY" || got[0].Value.IsSet() {
		t.Fatalf("read back %+v", got)
	}
}

// Only a pool, with the resolve scope, is told a value.
func TestResolveGatewayHeadersIsAPoolsAlone(t *testing.T) {
	svc := &fakeGatewayService{}
	h := New(svcapi.Services{Gateways: svc})
	body := &serverapi.ResolveGatewayHeadersBody{SandboxId: "sb-1", Gateway: "anthropic", Env: []string{"DISCOBOX_GATEWAY_KEY"}}
	params := serverapi.ResolveGatewayHeadersParams{PoolId: "pool-1"}
	for name, principal := range map[string]auth.Principal{
		"a user":           {Type: auth.PrincipalTypeUser, UserID: "user-1"},
		"a pool, unscoped": {Type: auth.PrincipalTypePool, PoolID: "pool-1"},
	} {
		res, err := h.ResolveGatewayHeaders(auth.WithPrincipal(context.Background(), principal), body, params)
		if err != nil {
			t.Fatal(err)
		}
		if status := responseStatus(t, res); status != http.StatusUnauthorized && status != http.StatusForbidden {
			t.Fatalf("%s answered %d", name, status)
		}
	}
	if svc.resolved != nil {
		t.Fatal("the service was asked for a caller that is not the pool")
	}
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{Type: auth.PrincipalTypePool, PoolID: "pool-1", Scopes: []string{poolauth.ScopeSecretResolve}})
	res, err := h.ResolveGatewayHeaders(ctx, body, params)
	if err != nil {
		t.Fatal(err)
	}
	answer, ok := res.(*serverapi.ResolveGatewayHeadersResponse)
	if !ok || answer.Values["DISCOBOX_GATEWAY_KEY"] != "gw-key" {
		t.Fatalf("response = %#v", res)
	}
}

func responseStatus(t *testing.T, res any) int {
	t.Helper()
	errRes, ok := res.(*serverapi.ErrorModelStatusCode)
	if !ok {
		t.Fatalf("response = %T, want an error", res)
	}
	return errRes.StatusCode
}
