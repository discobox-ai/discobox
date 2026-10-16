package gateways

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"gorm.io/gorm"

	"github.com/discobox-ai/discobox/proxy/gateway"
	"github.com/discobox-ai/discobox/server/internal/database"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// recordedMarks counts the pushes a change marks, in the transaction that
// made it.
type recordedMarks struct{ projects []string }

func (r *recordedMarks) MarkProxyPolicyChangedTx(_ context.Context, _ *store.Store, _ *gorm.DB, projectID, poolID, harnessConfigID string) error {
	if poolID != "" || harnessConfigID != "" {
		return errors.New("a gateway change marks the whole project")
	}
	r.projects = append(r.projects, projectID)
	return nil
}

func newTestService(t *testing.T) (*Service, *recordedMarks, map[string]string) {
	t.Helper()
	ctx := context.Background()
	db, err := database.New(database.Config{DSN: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for _, row := range []any{
		&model.Project{ID: "project-1", OwnerUserID: "user-1", Name: "Project"},
		&model.Project{ID: "project-2", OwnerUserID: "user-1", Name: "Other"},
		&model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Name: "provider", Type: "docker"},
		&model.Pool{ID: "pool-1", ProjectID: "project-1", PoolManifest: model.PoolManifest{ProviderInstanceID: "provider-1", Name: "pool"}},
		&model.Sandbox{ID: "sb-1", ProjectID: "project-1", PoolID: "pool-1", Name: "box"},
	} {
		if err := db.Write.WithContext(ctx).Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	marks := &recordedMarks{}
	env := map[string]string{"DISCOBOX_GATEWAY_KEY": "gw-key", "DISCOBOX_GATEWAY_EMPTY": ""}
	svc := NewService(store.New(db.Write, db.Read), marks)
	svc.lookupEnv = func(name string) (string, bool) {
		value, ok := env[name]
		return value, ok
	}
	return svc, marks, env
}

func anthropic() gateway.Route {
	return gateway.Route{
		Match:  "https://API.anthropic.com/v1/",
		Target: "https://gateway.example.com/anthropic/v1",
		RequestHeaders: gateway.HeaderModifier{
			Remove: []string{"x-api-key"},
			Set:    []gateway.Header{{Name: "cf-aig-authorization", Env: "DISCOBOX_GATEWAY_KEY"}},
		},
	}
}

func statusOf(err error) int {
	var statusErr interface{ StatusCode() int }
	if errors.As(err, &statusErr) {
		return statusErr.StatusCode()
	}
	return 0
}

// A put stores the gateway as the proxy reads it, replaces it in place by
// name, and marks the project's discoboxes each time; a delete does too.
func TestPutReplacesByNameAndMarksTheProject(t *testing.T) {
	ctx := context.Background()
	svc, marks, _ := newTestService(t)

	first, err := svc.PutProjectGateway(ctx, "project-1", "anthropic", anthropic())
	if err != nil {
		t.Fatal(err)
	}
	if first.Match != "https://api.anthropic.com/v1" || first.RequestHeaders.Remove[0] != "X-Api-Key" {
		t.Fatalf("stored %+v", first)
	}
	replacement := anthropic()
	replacement.Target = "https://other.example.com"
	second, err := svc.PutProjectGateway(ctx, "project-1", "anthropic", replacement)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID || second.Target != "https://other.example.com" || !second.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("replaced %+v, was %+v", second, first)
	}
	list, err := svc.ListProjectGateways(ctx, "project-1")
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if err := svc.DeleteProjectGateway(ctx, "project-1", "anthropic"); err != nil {
		t.Fatal(err)
	}
	if len(marks.projects) != 3 {
		t.Fatalf("marked %v, want one per change", marks.projects)
	}
	if err := svc.DeleteProjectGateway(ctx, "project-1", "anthropic"); statusOf(err) != http.StatusNotFound {
		t.Fatalf("delete again: %v", err)
	}
	if _, err := svc.GetProjectGateway(ctx, "project-1", "anthropic"); statusOf(err) != http.StatusNotFound {
		t.Fatalf("get after delete: %v", err)
	}
}

func TestPutRefuses(t *testing.T) {
	ctx := context.Background()
	svc, marks, _ := newTestService(t)
	if _, err := svc.PutProjectGateway(ctx, "project-1", "anthropic", anthropic()); err != nil {
		t.Fatal(err)
	}
	// Another name with the same match, however it is spelled.
	same := anthropic()
	same.Match = "https://api.anthropic.com:443/v1"
	if _, err := svc.PutProjectGateway(ctx, "project-1", "copy", same); statusOf(err) != http.StatusConflict {
		t.Fatalf("same match: %v", err)
	}
	// The same match in another project is that project's.
	if _, err := svc.PutProjectGateway(ctx, "project-2", "anthropic", anthropic()); err != nil {
		t.Fatalf("another project: %v", err)
	}
	bad := anthropic()
	bad.RequestHeaders.Set = []gateway.Header{{Name: "X", Env: "DATABASE_URL"}}
	if _, err := svc.PutProjectGateway(ctx, "project-1", "bad", bad); statusOf(err) != http.StatusBadRequest {
		t.Fatalf("an unprefixed variable: %v", err)
	}
	if _, err := svc.PutProjectGateway(ctx, "project-1", "Bad Name", anthropic()); statusOf(err) != http.StatusBadRequest {
		t.Fatalf("a bad name: %v", err)
	}
	if _, err := svc.PutProjectGateway(ctx, "missing", "anthropic", anthropic()); statusOf(err) != http.StatusNotFound {
		t.Fatalf("a missing project: %v", err)
	}
	if len(marks.projects) != 2 {
		t.Fatalf("marked %v, want only the two puts that stored", marks.projects)
	}
}

// A pool is answered only for its own sandbox, and only with the variables
// its project's gateway names; one the environment does not set, or sets
// empty, is left out.
func TestResolveGatewayHeaders(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newTestService(t)
	route := anthropic()
	route.RequestHeaders.Add = []gateway.Header{
		{Name: "X-Unset", Env: "DISCOBOX_GATEWAY_UNSET"},
		{Name: "X-Empty", Env: "DISCOBOX_GATEWAY_EMPTY"},
	}
	if _, err := svc.PutProjectGateway(ctx, "project-1", "anthropic", route); err != nil {
		t.Fatal(err)
	}
	values, err := svc.ResolveGatewayHeaders(ctx, "pool-1", "sb-1", "anthropic", []string{"DISCOBOX_GATEWAY_KEY", "DISCOBOX_GATEWAY_UNSET", "DISCOBOX_GATEWAY_EMPTY"})
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 || values["DISCOBOX_GATEWAY_KEY"] != "gw-key" {
		t.Fatalf("values = %v", values)
	}
	if _, err := svc.ResolveGatewayHeaders(ctx, "pool-2", "sb-1", "anthropic", []string{"DISCOBOX_GATEWAY_KEY"}); statusOf(err) != http.StatusNotFound {
		t.Fatalf("another pool: %v", err)
	}
	if _, err := svc.ResolveGatewayHeaders(ctx, "pool-1", "sb-1", "missing", []string{"DISCOBOX_GATEWAY_KEY"}); statusOf(err) != http.StatusNotFound {
		t.Fatalf("a missing gateway: %v", err)
	}
	if _, err := svc.ResolveGatewayHeaders(ctx, "pool-1", "sb-1", "anthropic", []string{"DISCOBOX_GATEWAY_OTHER"}); statusOf(err) != http.StatusForbidden {
		t.Fatalf("a variable the gateway does not name: %v", err)
	}
}
