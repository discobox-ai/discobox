package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/pkg/harnesscapture"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/services"
)

type captureErrorService struct{ services.HarnessConfigService }

func (captureErrorService) GetSandboxHarnessConfigChanges(context.Context, string, string) (harnesscapture.Preview, error) {
	return harnesscapture.Preview{}, apperrors.NewStatusError(http.StatusConflict, "upgrade or recreate the sandbox")
}
func (captureErrorService) DiscardSandboxHarnessConfigChanges(context.Context, string, string, harnesscapture.CandidateRequest) (harnesscapture.Preview, error) {
	return harnesscapture.Preview{}, apperrors.NewStatusError(http.StatusConflict, "upgrade or recreate the sandbox")
}

func TestCaptureErrorsDecodeWithGeneratedClient(t *testing.T) {
	handler, err := NewServer(services.Services{HarnessConfigs: captureErrorService{}})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	client, err := serverapi.NewClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	get, err := client.GetSandboxHarnessConfigChanges(context.Background(), serverapi.GetSandboxHarnessConfigChangesParams{ProjectId: "default", SandboxId: "box"})
	if err != nil {
		t.Fatal(err)
	}
	discard, err := client.DiscardSandboxHarnessConfigChanges(context.Background(), &serverapi.HarnessConfigCandidateRequest{CandidateId: "candidate"}, serverapi.DiscardSandboxHarnessConfigChangesParams{ProjectId: "default", SandboxId: "box"})
	if err != nil {
		t.Fatal(err)
	}
	for _, response := range []any{get, discard} {
		got, ok := response.(*serverapi.ErrorResponseStatusCode)
		if !ok || got.StatusCode != http.StatusConflict || got.Response.Error != "upgrade or recreate the sandbox" {
			t.Fatalf("response = %#v", response)
		}
	}
}

type captureSaveService struct {
	services.HarnessConfigService
	request  harnesscapture.CandidateRequest
	revision bool
}

func (s *captureSaveService) SaveSandboxHarnessConfigChanges(_ context.Context, _, _ string, r harnesscapture.CandidateRequest) (harnesscapture.SaveResult, error) {
	s.request = r
	if s.revision {
		return harnesscapture.SaveResult{}, apperrors.StatusError{Status: 409, Message: "revision changed", Cause: harnesscapture.ErrRevisionConflict}
	}
	return harnesscapture.SaveResult{}, apperrors.NewStatusError(409, "upgrade or recreate the sandbox")
}
func TestSaveConflictTypeAndForceRoundTrip(t *testing.T) {
	for _, revision := range []bool{false, true} {
		svc := &captureSaveService{revision: revision}
		handler, err := NewServer(services.Services{HarnessConfigs: svc})
		if err != nil {
			t.Fatal(err)
		}
		server := httptest.NewServer(handler)
		client, err := serverapi.NewClient(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		res, err := client.SaveSandboxHarnessConfigChanges(context.Background(), &serverapi.HarnessConfigCandidateRequest{CandidateId: "reviewed"}, serverapi.SaveSandboxHarnessConfigChangesParams{ProjectId: "default", SandboxId: "box", Force: serverapi.NewOptBool(true)})
		server.Close()
		if err != nil {
			t.Fatal(err)
		}
		problem, ok := res.(*serverapi.ErrorModelStatusCode)
		if !ok || problem.StatusCode != 409 {
			t.Fatalf("unexpected response: %#v", res)
		}
		if (problem.Response.Type.Value.String() == harnesscapture.RevisionConflictType) != revision {
			t.Fatalf("wrong conflict type: %v", problem.Response.Type)
		}
		if !svc.request.Force || svc.request.CandidateID != "reviewed" {
			t.Fatalf("request changed: %+v", svc.request)
		}
	}
}
