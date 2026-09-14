package apigen

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A dev CLI can select a released server whose pool responses still contain
// retired staging fields. Decode through the client used by pool reads.
func TestGetPoolAcceptsRetiredReleaseFields(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"pool-1","projectId":"project-1","name":"Pool",
			"providerInstanceId":"provider-1","cpuVcpus":2,"memoryBytes":1024,"storageBytes":2048,
			"ready":true,"schedulable":true,"degraded":false,
			"availableCpuVcpus":2,"availableMemoryBytes":1024,"availableStorageBytes":2048,
			"desiredState":"present","state":"active","generation":1,"observedGeneration":1,
			"createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z",
			"imagesStaged":true,"imageStage":"ready","imageStagedAt":"2026-01-01T00:00:00Z"
		}`))
	}))
	defer server.Close()
	client, err := NewClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.GetPool(context.Background(), GetPoolParams{ProjectId: "project-1", PoolId: "pool-1"})
	if err != nil {
		t.Fatal(err)
	}
	pool, ok := response.(*Pool)
	if !ok || pool.ID != "pool-1" || !pool.Ready {
		t.Fatalf("pool response = %#v", response)
	}
}
