package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/discobox-ai/discobox/server/internal/model"
)

func TestAPIAccessBootstrapOnlyAllowsOwnConsentRequests(t *testing.T) {
	st := roleStore(t)
	ctx := context.Background()
	req := &model.APIAccessRequest{ProjectID: "proj-1", RequesterSandboxID: "sbx-noapi", Justification: "Coordinate workers"}
	if err := st.CreateAPIAccessRequest(ctx, req); err != nil {
		t.Fatal(err)
	}
	a := SandboxRoleAuthorizer{Store: st}
	for _, tc := range []struct {
		box, method, path string
		allowed           bool
	}{
		{"sbx-noapi", "POST", "api-access-requests", true},
		{"sbx-noapi", "GET", "api-access-requests", true},
		{"sbx-noapi", "GET", "api-access-requests/" + req.ID, true},
		{"sbx-noapi", "POST", "api-access-requests/" + req.ID + "/approve", false},
		{"sbx-noapi", "POST", "api-access-requests/" + req.ID + "/deny", false},
		{"sbx-noapi", "GET", "sandboxes", false},
		{"sbx-noapi", "POST", "sandboxes", false},
		{"sbx-lead", "GET", "api-access-requests/" + req.ID, false},
		{"sbx-lead", "POST", "api-access-requests/" + req.ID + "/approve", false},
		{"sbx-noapi", "GET", "api-access-requests/" + req.ID + "/approve", false},
	} {
		p := Principal{Type: PrincipalTypeSandbox, SandboxID: tc.box, ProjectID: "proj-1", UserID: "user-1"}
		r := httptest.NewRequestWithContext(WithPrincipal(ctx, p), tc.method, "/projects/default/"+tc.path, nil)
		ok, err := a.Authorize(r)
		if (ok && err == nil) != tc.allowed {
			t.Errorf("%s %s %s = %t %v", tc.box, tc.method, tc.path, ok, err)
		}
	}
	// Turning API access off continues to revoke ordinary routes immediately.
	if _, err := st.DecideAPIAccessRequest(ctx, "proj-1", req.ID, "person", true); err != nil {
		t.Fatal(err)
	}
	p := Principal{Type: PrincipalTypeSandbox, SandboxID: "sbx-noapi", ProjectID: "proj-1", UserID: "user-1"}
	r := httptest.NewRequestWithContext(WithPrincipal(ctx, p), http.MethodGet, "/projects/default/sandboxes", nil)
	if ok, err := a.Authorize(r); !ok || err != nil {
		t.Fatalf("approved API access refused: %v", err)
	}
	if err := st.SetSandboxDiscoboxAPI(ctx, "proj-1", "sbx-noapi", false); err != nil {
		t.Fatal(err)
	}
	if ok, err := a.Authorize(r); ok || err == nil {
		t.Fatal("Settings revocation did not take effect")
	}
}
