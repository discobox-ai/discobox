package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/obot-platform/disco2/internal/api"
	"github.com/obot-platform/disco2/internal/model"
)

func TestCreateSandbox(t *testing.T) {
	h := newTestAPI(t).h

	created := createSandbox(t, h, "alpha")
	if created.ID == "" {
		t.Fatal("expected sandbox ID")
	}
	if created.DesiredState != model.SandboxDesiredStateRunning {
		t.Fatalf("created desired state = %q, want %q", created.DesiredState, model.SandboxDesiredStateRunning)
	}
	if created.Phase != model.SandboxPhasePending {
		t.Fatalf("created phase = %q, want %q", created.Phase, model.SandboxPhasePending)
	}
	if created.ActiveOperation == nil || *created.ActiveOperation != model.SandboxOperationCreate {
		t.Fatalf("created active operation = %v, want %q", created.ActiveOperation, model.SandboxOperationCreate)
	}
	if created.LastOperationStatus != model.SandboxOperationStatusPending {
		t.Fatalf("created operation status = %q, want %q", created.LastOperationStatus, model.SandboxOperationStatusPending)
	}
	if created.CreatedBy == nil {
		t.Fatal("expected createdBy to be populated")
	}
	if created.SourceURL == nil || *created.SourceURL != "https://example.com/repo.git" {
		t.Fatalf("sourceUrl = %v, want https://example.com/repo.git", created.SourceURL)
	}
}

func TestListSandboxes(t *testing.T) {
	h := newTestAPI(t).h
	created := createSandbox(t, h, "alpha")

	resp := h.Get(projectURL() + "/sandboxes")
	if resp.Code != http.StatusOK {
		t.Fatalf("list sandboxes status = %d, body = %s", resp.Code, resp.Body.String())
	}
	var list api.ListSandboxesBody
	if err := json.Unmarshal(resp.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode sandboxes: %v", err)
	}
	if len(list.Sandboxes) != 1 {
		t.Fatalf("sandboxes length = %d, want 1", len(list.Sandboxes))
	}
	if list.Sandboxes[0].ID != created.ID {
		t.Fatalf("sandbox ID = %q, want %q", list.Sandboxes[0].ID, created.ID)
	}
}

func TestGetSandbox(t *testing.T) {
	h := newTestAPI(t).h
	created := createSandbox(t, h, "alpha")

	resp := h.Get(sandboxURL(created.ID))
	if resp.Code != http.StatusOK {
		t.Fatalf("get sandbox status = %d, body = %s", resp.Code, resp.Body.String())
	}
	got := decodeSandbox(t, resp.Body.Bytes())
	if got.ID != created.ID {
		t.Fatalf("sandbox ID = %q, want %q", got.ID, created.ID)
	}
}

func TestUpdateSandbox(t *testing.T) {
	h := newTestAPI(t).h
	created := createSandbox(t, h, "alpha")

	resp := h.Patch(sandboxURL(created.ID), map[string]any{
		"name": "renamed",
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("update sandbox status = %d, body = %s", resp.Code, resp.Body.String())
	}
	updated := decodeSandbox(t, resp.Body.Bytes())
	if updated.Name != "renamed" {
		t.Fatalf("updated name = %q, want renamed", updated.Name)
	}
}

func TestStartSandbox(t *testing.T) {
	h := newTestAPI(t).h
	created := createSandbox(t, h, "alpha")

	resp := h.Post(sandboxURL(created.ID)+"/start", map[string]any{})
	if resp.Code != http.StatusOK {
		t.Fatalf("start sandbox status = %d, body = %s", resp.Code, resp.Body.String())
	}
	started := decodeSandbox(t, resp.Body.Bytes())
	if started.DesiredState != model.SandboxDesiredStateRunning {
		t.Fatalf("started desired state = %q, want %q", started.DesiredState, model.SandboxDesiredStateRunning)
	}
	if started.Phase != model.SandboxPhaseStarting {
		t.Fatalf("started phase = %q, want %q", started.Phase, model.SandboxPhaseStarting)
	}
	if started.ActiveOperation == nil || *started.ActiveOperation != model.SandboxOperationStart {
		t.Fatalf("started active operation = %v, want %q", started.ActiveOperation, model.SandboxOperationStart)
	}
}

func TestStopSandbox(t *testing.T) {
	h := newTestAPI(t).h
	created := createSandbox(t, h, "alpha")

	resp := h.Post(sandboxURL(created.ID)+"/stop", map[string]any{})
	if resp.Code != http.StatusOK {
		t.Fatalf("stop sandbox status = %d, body = %s", resp.Code, resp.Body.String())
	}
	stopped := decodeSandbox(t, resp.Body.Bytes())
	if stopped.DesiredState != model.SandboxDesiredStateStopped {
		t.Fatalf("stopped desired state = %q, want %q", stopped.DesiredState, model.SandboxDesiredStateStopped)
	}
	if stopped.Phase != model.SandboxPhaseStopping {
		t.Fatalf("stopped phase = %q, want %q", stopped.Phase, model.SandboxPhaseStopping)
	}
	if stopped.ActiveOperation == nil || *stopped.ActiveOperation != model.SandboxOperationStop {
		t.Fatalf("stopped active operation = %v, want %q", stopped.ActiveOperation, model.SandboxOperationStop)
	}
}

func TestRestartSandbox(t *testing.T) {
	h := newTestAPI(t).h
	created := createSandbox(t, h, "alpha")

	resp := h.Post(sandboxURL(created.ID)+"/restart", map[string]any{})
	if resp.Code != http.StatusOK {
		t.Fatalf("restart sandbox status = %d, body = %s", resp.Code, resp.Body.String())
	}
	restarted := decodeSandbox(t, resp.Body.Bytes())
	if restarted.DesiredState != model.SandboxDesiredStateRunning {
		t.Fatalf("restarted desired state = %q, want %q", restarted.DesiredState, model.SandboxDesiredStateRunning)
	}
	if restarted.Phase != model.SandboxPhaseStarting {
		t.Fatalf("restarted phase = %q, want %q", restarted.Phase, model.SandboxPhaseStarting)
	}
	if restarted.ActiveOperation == nil || *restarted.ActiveOperation != model.SandboxOperationRestart {
		t.Fatalf("restarted active operation = %v, want %q", restarted.ActiveOperation, model.SandboxOperationRestart)
	}
}

func TestDeleteSandbox(t *testing.T) {
	h := newTestAPI(t).h
	created := createSandbox(t, h, "alpha")

	resp := h.Delete(sandboxURL(created.ID))
	if resp.Code != http.StatusNoContent {
		t.Fatalf("delete sandbox status = %d, body = %s", resp.Code, resp.Body.String())
	}

	resp = h.Get(sandboxURL(created.ID))
	if resp.Code != http.StatusOK {
		t.Fatalf("get deleting sandbox status = %d, want %d, body = %s", resp.Code, http.StatusOK, resp.Body.String())
	}
	deleting := decodeSandbox(t, resp.Body.Bytes())
	if deleting.DesiredState != model.SandboxDesiredStateDeleted {
		t.Fatalf("deleted desired state = %q, want %q", deleting.DesiredState, model.SandboxDesiredStateDeleted)
	}
	if deleting.Phase != model.SandboxPhaseDeleting {
		t.Fatalf("deleted phase = %q, want %q", deleting.Phase, model.SandboxPhaseDeleting)
	}
}
