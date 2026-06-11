package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/danielgtaylor/huma/v2/humatest"

	"github.com/obot-platform/disco2/internal/api"
	"github.com/obot-platform/disco2/internal/model"
)

func TestWorkerStatusPassesAuthorizationHeader(t *testing.T) {
	handler, h := humatest.New(t)
	service := &recordingWorkerService{}
	api.RegisterWorkerOperations(h, service)
	body, err := json.Marshal(map[string]any{
		"tenantId":              "tenant-1",
		"workerId":              "worker-1",
		"ready":                 true,
		"schedulable":           true,
		"degraded":              false,
		"availableCpuVcpus":     1,
		"availableMemoryBytes":  1024,
		"availableStorageBytes": 2048,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/workers/status", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer token-1")
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("status code = %d body = %s", resp.Code, resp.Body.String())
	}
	if service.authorization != "Bearer token-1" {
		t.Fatalf("authorization = %q, want bearer header", service.authorization)
	}
}

type recordingWorkerService struct {
	authorization string
}

func (s *recordingWorkerService) RegisterWorker(context.Context, api.RegisterWorkerBody) (*api.RegisterWorkerResponseBody, error) {
	return &api.RegisterWorkerResponseBody{AuthToken: "token"}, nil
}

func (s *recordingWorkerService) UpdateWorkerStatus(_ context.Context, authorization string, input api.UpdateWorkerStatusBody) (*model.Worker, error) {
	s.authorization = authorization
	return &model.Worker{ID: input.WorkerID, TenantID: input.TenantID, Ready: input.Ready, Schedulable: input.Schedulable}, nil
}
