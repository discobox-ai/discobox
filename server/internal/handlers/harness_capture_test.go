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
	request harnesscapture.CandidateRequest
	fail    bool
}

func (s *captureSaveService) SaveSandboxHarnessConfigChanges(_ context.Context, _, _ string, r harnesscapture.CandidateRequest) (harnesscapture.SaveResult, error) {
	s.request = r
	if s.fail {
		return harnesscapture.SaveResult{}, apperrors.NewStatusError(409, "upgrade or recreate the sandbox")
	}
	return harnesscapture.SaveResult{CandidateID: r.CandidateID, HarnessConfigID: "harness", ConfigRevision: 2, State: "saved"}, nil
}

func TestSaveCandidateRoundTrip(t *testing.T) {
	for _, fail := range []bool{false, true} {
		svc := &captureSaveService{fail: fail}
		handler, err := NewServer(services.Services{HarnessConfigs: svc})
		if err != nil {
			t.Fatal(err)
		}
		server := httptest.NewServer(handler)
		client, err := serverapi.NewClient(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		res, err := client.SaveSandboxHarnessConfigChanges(context.Background(), &serverapi.HarnessConfigCandidateRequest{CandidateId: "reviewed"}, serverapi.SaveSandboxHarnessConfigChangesParams{ProjectId: "default", SandboxId: "box"})
		server.Close()
		if err != nil {
			t.Fatal(err)
		}
		if fail {
			problem, ok := res.(*serverapi.ErrorModelStatusCode)
			if !ok || problem.StatusCode != 409 || problem.Response.Detail.Or("") != "upgrade or recreate the sandbox" {
				t.Fatalf("lost recovery: %#v", res)
			}
		} else {
			saved, ok := res.(*serverapi.HarnessConfigSaveResult)
			if !ok || saved.State != "saved" || saved.CandidateId != "reviewed" {
				t.Fatalf("wrong save result: %#v", res)
			}
		}
		if svc.request.CandidateID != "reviewed" {
			t.Fatalf("candidate changed: %+v", svc.request)
		}
	}
}
