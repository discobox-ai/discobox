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
