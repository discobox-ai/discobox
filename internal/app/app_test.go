package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewRouterServesOpenAPIAndSwaggerDocs(t *testing.T) {
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
	if body := docsResp.Body.String(); !strings.Contains(body, "SwaggerUIBundle") {
		t.Fatalf("GET /docs body does not look like Swagger UI")
	}
	if body := docsResp.Body.String(); !strings.Contains(body, "/openapi.json") {
		t.Fatalf("GET /docs body does not reference /openapi.json")
	}
}
