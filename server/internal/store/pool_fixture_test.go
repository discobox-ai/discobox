package store_test

import (
	"context"
	"testing"

	"github.com/obot-platform/discobox/server/internal/model"
	"github.com/obot-platform/discobox/server/internal/store"
)

// createTestPool seeds the project, provider instance, and pool rows a sandbox
// or worker fixture needs: pool references are enforced by foreign keys, so a
// row cannot dangle from a pool that does not exist.
func createTestPool(t *testing.T, s *store.Store, projectID, poolID string) {
	t.Helper()
	ctx := context.Background()
	if err := s.UpsertProject(ctx, &model.Project{ID: projectID, OwnerUserID: "user-1", Name: projectID, Slug: projectID}); err != nil {
		t.Fatalf("create project %s: %v", projectID, err)
	}
	providerID := "prov-" + poolID
	if _, err := s.GetSandboxProviderInstance(ctx, projectID, providerID); err != nil {
		if err := s.CreateSandboxProviderInstance(ctx, &model.SandboxProviderInstance{ID: providerID, ProjectID: projectID, Type: "docker", Name: providerID}); err != nil {
			t.Fatalf("create provider %s: %v", providerID, err)
		}
	}
	if _, err := s.GetPool(ctx, projectID, poolID); err != nil {
		if err := s.CreatePool(ctx, &model.Pool{ID: poolID, ProjectID: projectID, Name: poolID, ProviderInstanceID: providerID}); err != nil {
			t.Fatalf("create pool %s: %v", poolID, err)
		}
	}
}
