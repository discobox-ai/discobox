package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/danielgtaylor/huma/v2/humatest"

	"github.com/obot-platform/discobox/model"
	"github.com/obot-platform/discobox/orchestration"
	"github.com/obot-platform/discobox/server/internal/api"
	"github.com/obot-platform/discobox/server/internal/database"
	"github.com/obot-platform/discobox/server/internal/events"
	"github.com/obot-platform/discobox/server/internal/service"
	"github.com/obot-platform/discobox/server/internal/store"
)

type testAPI struct {
	handler http.Handler
	h       humatest.TestAPI
}

func newTestAPI(t *testing.T) testAPI {
	t.Helper()

	ctx := context.Background()
	db, err := database.New(database.Config{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close db: %v", err)
		}
	})
	if err := db.MigrateTenant(ctx); err != nil {
		t.Fatalf("migrate db: %v", err)
	}

	broker := events.NewBroker()
	appStore := store.New(database.StaticResolver{DB: db}, store.WithPublisher(broker), store.WithDefaultTenantID(service.DefaultTenantID))
	queueConfig := orchestration.QueueConfig{DefaultMaxAttempts: 3}
	services := service.New(appStore, queueConfig, nil, broker)
	if err := services.InitializeDefaults(ctx, service.DefaultTenantID, service.DefaultUserID); err != nil {
		t.Fatalf("initialize defaults: %v", err)
	}

	handler, h := humatest.New(t)
	api.Register(h, api.Services{
		Projects:     services,
		AgentConfigs: services,
		Sandboxes:    services,
		Providers:    services,
		Workers:      services,
		Events:       services,
	})
	return testAPI{handler: handler, h: h}
}

func createSandbox(t *testing.T, h humatest.TestAPI, name string) model.Sandbox {
	t.Helper()

	resp := h.Post(projectURL()+"/sandboxes", map[string]any{
		"name":             name,
		"description":      "test sandbox",
		"sourceUrl":        "https://example.com/repo.git",
		"sourceRef":        "main",
		"sourceDirectory":  "/workspace",
		"workingDirectory": "/workspace",
	})
	if resp.Code != http.StatusAccepted {
		t.Fatalf("create sandbox status = %d, body = %s", resp.Code, resp.Body.String())
	}
	return decodeSandbox(t, resp.Body.Bytes())
}

func decodeSandbox(t *testing.T, data []byte) model.Sandbox {
	t.Helper()

	var sandbox model.Sandbox
	if err := json.Unmarshal(data, &sandbox); err != nil {
		t.Fatalf("decode sandbox: %v", err)
	}
	return sandbox
}

func projectURL() string {
	return "/projects/" + service.DefaultProjectID
}

func sandboxURL(sandboxID string) string {
	return projectURL() + "/sandboxes/" + sandboxID
}
