package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/obot-platform/disco2/internal/api"
	"github.com/obot-platform/disco2/internal/model"
	"github.com/obot-platform/disco2/internal/service"
)

func TestProjectOperations(t *testing.T) {
	apiFixture := newTestAPI(t)
	h := apiFixture.h

	resp := h.Get("/projects")
	if resp.Code != http.StatusOK {
		t.Fatalf("GET /projects status = %d, body = %s", resp.Code, resp.Body.String())
	}

	var list api.ListProjectsBody
	if err := json.Unmarshal(resp.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode projects: %v", err)
	}
	if len(list.Projects) != 1 {
		t.Fatalf("projects length = %d, want 1", len(list.Projects))
	}
	if list.Projects[0].ID != service.DefaultProjectID {
		t.Fatalf("project ID = %q, want %q", list.Projects[0].ID, service.DefaultProjectID)
	}

	resp = h.Get("/projects/" + service.DefaultProjectID)
	if resp.Code != http.StatusOK {
		t.Fatalf("GET /projects/{id} status = %d, body = %s", resp.Code, resp.Body.String())
	}

	var project model.Project
	if err := json.Unmarshal(resp.Body.Bytes(), &project); err != nil {
		t.Fatalf("decode project: %v", err)
	}
	if project.Owner == nil {
		t.Fatal("expected project owner to be populated")
	}
}
