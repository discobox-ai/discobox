package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/discobox-ai/discobox/server/internal/model"
)

func TestSandboxLeadCanRunAndAttachExecsInCreatedAndAdoptedChildren(t *testing.T) {
	st := roleStore(t)
	ctx := context.Background()
	req := &model.AdoptionRequest{ProjectID: "proj-1", RequesterSandboxID: "sbx-lead", ChildSandboxID: "sbx-persons", Justification: "manage work"}
	if err := st.CreateAdoptionRequest(ctx, req); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DecideAdoptionRequest(ctx, "proj-1", req.ID, "user-1", true); err != nil {
		t.Fatal(err)
	}
	authorizer := SandboxRoleAuthorizer{Store: st}
	for _, child := range []string{"sbx-worker", "sbx-persons"} {
		for _, route := range []struct{ method, suffix string }{
			{http.MethodPost, "/execs"},
			{http.MethodGet, "/execs/exec-1/attach"},
		} {
			t.Run(child+route.suffix, func(t *testing.T) {
				path := "/projects/default/sandboxes/" + child + route.suffix
				r := httptest.NewRequestWithContext(WithPrincipal(ctx, roleLead), route.method, path, nil)
				ok, err := authorizer.Authorize(r)
				if !ok || err != nil {
					t.Fatalf("lead must be allowed %s %s: allowed=%v, err=%v", route.method, path, ok, err)
				}
			})
		}
	}
}
