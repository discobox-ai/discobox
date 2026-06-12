package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/obot-platform/discobox/gormdb"
	"github.com/obot-platform/discobox/internal/database"
	"github.com/obot-platform/discobox/internal/model"
	"github.com/obot-platform/discobox/internal/service"
)

func TestNewRouterServesOpenAPIAndScalarDocs(t *testing.T) {
	router, _ := NewStubbedRouter()

	openapiResp := httptest.NewRecorder()
	router.ServeHTTP(openapiResp, httptest.NewRequest(http.MethodGet, "/openapi.json", nil))
	if openapiResp.Code != http.StatusOK {
		t.Fatalf("GET /openapi.json status = %d, want %d", openapiResp.Code, http.StatusOK)
	}
	if contentType := openapiResp.Header().Get("Content-Type"); !strings.Contains(contentType, "application/openapi+json") {
		t.Fatalf("GET /openapi.json content type = %q, want OpenAPI JSON", contentType)
	}
	if body := openapiResp.Body.String(); !strings.Contains(body, `"openapi"`) {
		t.Fatalf("GET /openapi.json body does not look like an OpenAPI document")
	}

	docsResp := httptest.NewRecorder()
	router.ServeHTTP(docsResp, httptest.NewRequest(http.MethodGet, "/docs", nil))
	if docsResp.Code != http.StatusOK {
		t.Fatalf("GET /docs status = %d, want %d", docsResp.Code, http.StatusOK)
	}
	if body := docsResp.Body.String(); !strings.Contains(body, "@scalar/api-reference") {
		t.Fatalf("GET /docs body does not look like Scalar")
	}
	if body := docsResp.Body.String(); !strings.Contains(body, "/openapi.json") {
		t.Fatalf("GET /docs body does not reference /openapi.json")
	}
}

func TestNewDatabaseRouterFallsBackToDefaultTenant(t *testing.T) {
	ctx := context.Background()
	resolver := database.NewResolver(database.ResolverConfig{
		Config: database.Config{
			Driver: gormdb.DriverSQLite,
			DSN:    "sqlite3://" + filepath.Join(t.TempDir(), "discobox.db"),
		},
		MigrateOnOpen: true,
	})
	t.Cleanup(func() {
		if err := resolver.Close(); err != nil {
			t.Fatalf("close resolver: %v", err)
		}
	})

	router, _, err := NewDatabaseRouter(ctx, resolver, DatabaseRouterOptions{
		DispatcherEnabled: false,
	})
	if err != nil {
		t.Fatalf("new database router: %v", err)
	}

	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/projects", nil))
	if resp.Code != http.StatusOK {
		t.Fatalf("GET /projects status = %d, body = %s", resp.Code, resp.Body.String())
	}

	var body struct {
		Projects []model.Project `json:"projects"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode projects: %v", err)
	}
	if len(body.Projects) != 1 {
		t.Fatalf("projects len = %d, want 1", len(body.Projects))
	}
	if body.Projects[0].TenantID != service.DefaultTenantID {
		t.Fatalf("tenant ID = %q, want %q", body.Projects[0].TenantID, service.DefaultTenantID)
	}
}

func TestNewDatabaseRouterResolvesDefaultProjectAlias(t *testing.T) {
	ctx := context.Background()
	resolver := database.NewResolver(database.ResolverConfig{
		Config: database.Config{
			Driver: gormdb.DriverSQLite,
			DSN:    "sqlite3://" + filepath.Join(t.TempDir(), "discobox.db"),
		},
		MigrateOnOpen: true,
	})
	t.Cleanup(func() {
		if err := resolver.Close(); err != nil {
			t.Fatalf("close resolver: %v", err)
		}
	})

	router, _, err := NewDatabaseRouter(ctx, resolver, DatabaseRouterOptions{
		DispatcherEnabled: false,
	})
	if err != nil {
		t.Fatalf("new database router: %v", err)
	}

	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/projects/default", nil))
	if resp.Code != http.StatusOK {
		t.Fatalf("GET /projects/default status = %d, body = %s", resp.Code, resp.Body.String())
	}

	var project model.Project
	if err := json.Unmarshal(resp.Body.Bytes(), &project); err != nil {
		t.Fatalf("decode project: %v", err)
	}
	if project.ID != service.DefaultProjectID {
		t.Fatalf("project ID = %q, want %q", project.ID, service.DefaultProjectID)
	}
	if !project.Default {
		t.Fatal("expected default project flag")
	}
}
