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

func (f *fakeGatewayService) GetProjectGateway(context.Context, string, string) (*model.ProjectGateway, error) {
	return nil, nil
}

func (f *fakeGatewayService) PutProjectGateway(_ context.Context, projectID, name string, route gateway.Route) (*model.ProjectGateway, error) {
	f.put = route
	return &model.ProjectGateway{
		ProjectID: projectID, Name: name, Match: route.Match, Target: route.Target,
		RequestHeaders: route.RequestHeaders, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}, nil
}

func (f *fakeGatewayService) DeleteProjectGateway(context.Context, string, string) error { return nil }

func (f *fakeGatewayService) ResolveGatewayHeaders(_ context.Context, _, _, _ string, env []string) (map[string]string, error) {
	f.resolved = env
	return map[string]string{"DISCOBOX_GATEWAY_KEY": "gw-key"}, nil
}

// A header's value is either written or taken from the server's environment,
// and reads back the way it was put: the variable's name, never its value.
func TestPutProjectGatewayReadsValueFrom(t *testing.T) {
	svc := &fakeGatewayService{}
	h := New(svcapi.Services{Gateways: svc})
	res, err := h.PutProjectGateway(context.Background(), &serverapi.PutProjectGatewayBody{
		Match:  "https://api.anthropic.com/v1",
		Target: "https://gw.example.com/anthropic/v1",
		RequestHeaders: serverapi.NewOptGatewayHeaderModifier(serverapi.GatewayHeaderModifier{
			Remove: []string{"X-Api-Key"},
			Set: []serverapi.GatewayHeader{
				{Name: "Cf-Aig-Authorization", ValueFrom: serverapi.NewOptGatewayHeaderValueSource(serverapi.GatewayHeaderValueSource{Env: "DISCOBOX_GATEWAY_KEY"})},
				{Name: "X-Team", Value: serverapi.NewOptString("platform")},
			},
		}),
	}, serverapi.PutProjectGatewayParams{ProjectId: "project-1", GatewayName: "anthropic"})
	if err != nil {
		t.Fatal(err)
	}
	set := svc.put.RequestHeaders.Set
	if len(set) != 2 || set[0].Env != "DISCOBOX_GATEWAY_KEY" || set[0].Value != "" || set[1].Value != "platform" {
		t.Fatalf("put %+v", svc.put)
	}
	body, ok := res.(*serverapi.ProjectGateway)
	if !ok {
		t.Fatalf("response = %T", res)
	}
	got := body.RequestHeaders.Set
	if got[0].ValueFrom.Or(serverapi.GatewayHeaderValueSource{}).Env != "DISCOBOX_GATEWAY_KEY" || got[0].Value.IsSet() || got[1].Value.Or("") != "platform" {
		t.Fatalf("read back %+v", got)
	}
}

func TestPutProjectGatewayRefusesAHeaderWithBothOrNeither(t *testing.T) {
	h := New(svcapi.Services{Gateways: &fakeGatewayService{}})
	for _, header := range []serverapi.GatewayHeader{
		{Name: "X"},
		{Name: "X", Value: serverapi.NewOptString("v"), ValueFrom: serverapi.NewOptGatewayHeaderValueSource(serverapi.GatewayHeaderValueSource{Env: "DISCOBOX_GATEWAY_KEY"})},
	} {
		res, err := h.PutProjectGateway(context.Background(), &serverapi.PutProjectGatewayBody{
			Match: "https://a.example.com", Target: "https://b.example.com",
			RequestHeaders: serverapi.NewOptGatewayHeaderModifier(serverapi.GatewayHeaderModifier{Add: []serverapi.GatewayHeader{header}}),
		}, serverapi.PutProjectGatewayParams{ProjectId: "project-1", GatewayName: "g"})
		if err != nil {
			t.Fatal(err)
		}
		if status := responseStatus(t, res); status != http.StatusBadRequest {
			t.Fatalf("%+v answered %d", header, status)
		}
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
