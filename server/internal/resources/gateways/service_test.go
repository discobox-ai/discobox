package gateways

import (
	"context"
	"errors"
	"net/http"
	"strings"
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
	svc := NewService(store.New(db.Write, db.Read), marks, Configured{}, "user-1")
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
			Set:    []gateway.Header{{Name: "x-team", Value: "platform"}},
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
	// A value from the server's environment is the configuration file's
	// alone (ADR 26-10-17-271 §3).
	fromEnv := anthropic()
	fromEnv.Match = "https://api.openai.com/v1"
	fromEnv.RequestHeaders.Set = []gateway.Header{{Name: "Authorization", Env: "DISCOBOX_GATEWAY_KEY"}}
	if _, err := svc.PutProjectGateway(ctx, "project-1", "env", fromEnv); statusOf(err) != http.StatusBadRequest {
		t.Fatalf("a header from the environment: %v", err)
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

// A pool is answered only for its own sandbox, only for a gateway the
// configuration file keeps, and only with the variables that gateway names;
// one the environment does not set, or sets empty, is left out.
func TestResolveGatewayHeaders(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newTestService(t)
	configured := anthropic()
	configured.Name = "anthropic"
	configured.RequestHeaders.Set = []gateway.Header{{Name: "Cf-Aig-Authorization", Env: "DISCOBOX_GATEWAY_KEY"}}
	configured.RequestHeaders.Add = []gateway.Header{
		{Name: "X-Unset", Env: "DISCOBOX_GATEWAY_UNSET"},
		{Name: "X-Empty", Env: "DISCOBOX_GATEWAY_EMPTY"},
	}
	configure(t, svc, Configured{Default: []gateway.Route{gateway.Normalize(configured)}})
	apply(t, svc)
	if _, err := svc.PutProjectGateway(ctx, "project-1", "mine", route("mine", "https://example.com", "https://member.example.com")); err != nil {
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
	if _, err := svc.ResolveGatewayHeaders(ctx, "pool-1", "sb-1", "mine", []string{"DISCOBOX_GATEWAY_KEY"}); statusOf(err) != http.StatusForbidden {
		t.Fatalf("a gateway the API made: %v", err)
	}
}

// configure makes svc's configuration file name configured, with user-1's
// default project project-1.
func configure(t *testing.T, svc *Service, configured Configured) {
	t.Helper()
	ctx := context.Background()
	for _, projectID := range []string{"project-1", "project-2"} {
		if _, err := svc.store.CreateProjectMemberIfNotExists(ctx, &model.ProjectMember{ProjectID: projectID, UserID: "user-1", Role: "owner"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.store.SetDefaultProjectForUser(ctx, "user-1", "project-1"); err != nil {
		t.Fatal(err)
	}
	svc.configured = configured
}

// apply applies svc's configuration file in a transaction of its own, as
// startup does.
func apply(t *testing.T, svc *Service) {
	t.Helper()
	ctx := context.Background()
	if err := svc.store.Transaction(ctx, func(txStore *store.Store, txDB *gorm.DB) error {
		return svc.ApplyConfiguredGatewaysTx(ctx, txStore, txDB)
	}); err != nil {
		t.Fatal(err)
	}
}

func route(name, match, target string) gateway.Route {
	return gateway.Normalize(gateway.Route{Name: name, Match: match, Target: target})
}

func gatewaysOf(t *testing.T, svc *Service, projectID string) map[string]model.ProjectGateway {
	t.Helper()
	list, err := svc.ListProjectGateways(context.Background(), projectID)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]model.ProjectGateway{}
	for _, gw := range list {
		out[gw.Name] = gw
	}
	return out
}

// The file's gateways are put on the projects its handles name, over the
// API's of the same name or match, and leave the API's others alone; the API
// may then read them and not change them (ADR 26-10-17-271 §3).
func TestConfiguredGatewaysAreTheFiles(t *testing.T) {
	ctx := context.Background()
	svc, marks, _ := newTestService(t)
	for name, match := range map[string]string{"anthropic": "https://api.anthropic.com/v1", "openai": "https://api.openai.com/v1", "mine": "https://example.com"} {
		if _, err := svc.PutProjectGateway(ctx, "project-1", name, route(name, match, "https://member.example.com")); err != nil {
			t.Fatal(err)
		}
	}
	configure(t, svc, Configured{
		Default: []gateway.Route{
			route("anthropic", "https://api.anthropic.com/v1", "https://gateway.example.com/anthropic"),
			route("llm", "https://api.openai.com/v1", "https://gateway.example.com/openai"),
		},
		ByProject: map[string][]gateway.Route{
			"project-2":             {route("anthropic", "https://api.anthropic.com/v1", "https://gateway.example.com/two")},
			"proj_6q3w8y5z7k9m4n6p": {route("gone", "https://gone.example.com", "https://gateway.example.com")},
		},
	})
	marks.projects = nil

	apply(t, svc)
	one := gatewaysOf(t, svc, "project-1")
	if len(one) != 3 {
		t.Fatalf("project-1 has %v, want anthropic, llm and mine", one)
	}
	if gw := one["anthropic"]; gw.ManagedBy != model.GatewayManagedByConfig || gw.Target != "https://gateway.example.com/anthropic" {
		t.Fatalf("anthropic = %+v, want the file's, taken over", gw)
	}
	if gw := one["llm"]; gw.ManagedBy != model.GatewayManagedByConfig {
		t.Fatalf("llm = %+v, want the file's", gw)
	}
	if gw := one["mine"]; gw.ManagedBy != model.GatewayManagedByAPI || gw.Target != "https://member.example.com" {
		t.Fatalf("mine = %+v, want the API's, untouched", gw)
	}
	if gw := gatewaysOf(t, svc, "project-2")["anthropic"]; gw.Target != "https://gateway.example.com/two" {
		t.Fatalf("project-2 anthropic = %+v", gw)
	}
	if len(marks.projects) != 2 {
		t.Fatalf("marked %v, want each project once", marks.projects)
	}

	// Once applied, applying again changes nothing.
	marks.projects = nil
	apply(t, svc)
	if again := gatewaysOf(t, svc, "project-1"); again["anthropic"].ID != one["anthropic"].ID || len(marks.projects) != 0 {
		t.Fatalf("applying again rewrote %+v and marked %v", again, marks.projects)
	}

	if _, err := svc.PutProjectGateway(ctx, "project-1", "anthropic", anthropic()); statusOf(err) != http.StatusConflict || !strings.Contains(err.Error(), "server.yaml") {
		t.Fatalf("put over the file's: %v", err)
	}
	// Over the file's name with the API's match, the refusal is still the
	// file's.
	if _, err := svc.PutProjectGateway(ctx, "project-1", "anthropic", route("anthropic", "https://example.com", "https://x.example.com")); statusOf(err) != http.StatusConflict || !strings.Contains(err.Error(), "server.yaml") {
		t.Fatalf("put over the file's name with the API's match: %v", err)
	}
	if _, err := svc.PutProjectGateway(ctx, "project-1", "copy", route("copy", "https://api.openai.com/v1", "https://x.example.com")); statusOf(err) != http.StatusConflict {
		t.Fatalf("put with the file's match: %v", err)
	}
	if err := svc.DeleteProjectGateway(ctx, "project-1", "llm"); statusOf(err) != http.StatusConflict {
		t.Fatalf("delete the file's: %v", err)
	}
	if err := svc.DeleteProjectGateway(ctx, "project-1", "mine"); err != nil {
		t.Fatalf("delete the API's: %v", err)
	}
}

// A gateway the file no longer keeps on a project is taken away: when the
// file stops naming it, and when the default handle names another project
// (ADR 26-10-17-271 §3, §4).
func TestConfiguredGatewaysFollowTheFileAndTheDefault(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newTestService(t)
	configure(t, svc, Configured{
		Default: []gateway.Route{
			route("anthropic", "https://api.anthropic.com/v1", "https://gateway.example.com/anthropic"),
			route("llm", "https://api.openai.com/v1", "https://gateway.example.com/openai"),
		},
	})
	apply(t, svc)

	svc.configured.Default = svc.configured.Default[:1]
	apply(t, svc)
	if one := gatewaysOf(t, svc, "project-1"); len(one) != 1 || one["anthropic"].ManagedBy != model.GatewayManagedByConfig {
		t.Fatalf("project-1 has %v, want anthropic alone", one)
	}

	if _, err := svc.store.SetDefaultProjectForUser(ctx, "user-1", "project-2"); err != nil {
		t.Fatal(err)
	}
	apply(t, svc)
	if one := gatewaysOf(t, svc, "project-1"); len(one) != 0 {
		t.Fatalf("project-1 kept %v after the default moved", one)
	}
	if two := gatewaysOf(t, svc, "project-2"); len(two) != 1 {
		t.Fatalf("project-2 has %v, want the default's", two)
	}

	// The same project named as default and by its ID takes the entry under
	// its ID.
	svc.configured.ByProject = map[string][]gateway.Route{"project-2": {route("byid", "https://by.example.com", "https://gateway.example.com")}}
	apply(t, svc)
	if two := gatewaysOf(t, svc, "project-2"); len(two) != 1 || two["byid"].Name == "" {
		t.Fatalf("project-2 has %v, want the entry under its ID", two)
	}
}

// A gateway the API made with a header from the server's environment, from
// before only the file could name one, is deleted when the file is applied,
// and its project's discoboxes are marked so their pools drop it and any value
// they hold for it (ADR 26-10-17-271 §3).
func TestApplyDeletesTheAPIsGatewaysFromTheEnvironment(t *testing.T) {
	ctx := context.Background()
	svc, marks, _ := newTestService(t)
	old := route("old", "https://api.openai.com/v1", "https://attacker.example.com")
	old.RequestHeaders.Set = []gateway.Header{{Name: "Authorization", Env: "DISCOBOX_GATEWAY_KEY"}}
	if err := svc.store.PutProjectGateway(ctx, &model.ProjectGateway{ProjectID: "project-2", Name: old.Name, Match: old.Match, Target: old.Target, RequestHeaders: old.RequestHeaders}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PutProjectGateway(ctx, "project-2", "mine", route("mine", "https://example.com", "https://member.example.com")); err != nil {
		t.Fatal(err)
	}
	configure(t, svc, Configured{})
	marks.projects = nil

	apply(t, svc)
	two := gatewaysOf(t, svc, "project-2")
	if _, kept := two["old"]; kept || len(two) != 1 {
		t.Fatalf("project-2 has %v, want mine alone", two)
	}
	if len(marks.projects) != 1 || marks.projects[0] != "project-2" {
		t.Fatalf("marked %v, want project-2", marks.projects)
	}
}
