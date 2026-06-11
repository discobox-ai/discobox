package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2/humatest"

	"github.com/obot-platform/disco2/internal/api"
	"github.com/obot-platform/disco2/internal/database"
	"github.com/obot-platform/disco2/internal/events"
	"github.com/obot-platform/disco2/internal/model"
	"github.com/obot-platform/disco2/internal/orchestration"
	"github.com/obot-platform/disco2/internal/service"
	"github.com/obot-platform/disco2/internal/store"
	"github.com/obot-platform/disco2/jobqueue"
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
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate db: %v", err)
	}

	broker := events.NewBroker()
	appStore := store.New(database.StaticResolver{DB: db}, store.WithPublisher(broker), store.WithDefaultTenantID(service.DefaultTenantID))
	queueConfig := jobqueue.QueueConfig{DefaultMaxAttempts: 3}
	ensureJob := func(ctx context.Context, txStore *store.Store, payload jobqueue.Payload) (*jobqueue.Job, bool, error) {
		return txStore.EnsureActiveJobForPayload(ctx, payload, queueConfig)
	}
	services := service.New(appStore, orchestration.New(appStore, ensureJob, nil), broker)
	if err := services.InitializeDefaults(ctx, service.DefaultTenantID, service.DefaultUserID); err != nil {
		t.Fatalf("initialize defaults: %v", err)
	}

	handler, h := humatest.New(t)
	api.Register(h, api.Services{
		Projects:  services,
		Sandboxes: services,
		Providers: services,
		Workers:   services,
		Events:    services,
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
		"workingDirectory": "/workspace",
		"runtimeState": map[string]any{
			"image": "alpine",
		},
	})
	if resp.Code != http.StatusOK {
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

func containsAll(s string, needles ...string) bool {
	for _, needle := range needles {
		if !strings.Contains(s, needle) {
			return false
		}
	}
	return true
}
