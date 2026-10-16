package pools

import (
	"context"
	"testing"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
	"gorm.io/gorm"
)

// recordedEgress stands in for the sandbox service, recording which pools an
// egress change was converged for.
type recordedEgress struct{ marked []string }

func (r *recordedEgress) MarkProxyPolicyChangedTx(_ context.Context, _ *store.Store, _ *gorm.DB, projectID, poolID, harnessConfigID string) error {
	r.marked = append(r.marked, projectID+"/"+poolID+"/"+harnessConfigID)
	return nil
}

// A pool's egress level is stored, converges the pool's discoboxes alone, and
// refuses an entry the proxy could not enforce.
func TestUpdatePoolSetsTheEgressLevel(t *testing.T) {
	svc := newSizeFixture(t)
	ctx := context.Background()
	recorded := svc.sandboxEgress.(*recordedEgress)
	if err := svc.store.CreatePool(ctx, &model.Pool{ID: "pool-1", ProjectID: "project-1", PoolManifest: model.PoolManifest{
		Name: "pool-1", ProviderInstanceID: "prov-unsized",
	}}); err != nil {
		t.Fatalf("store pool: %v", err)
	}
	level := func(allow ...string) services.UpdatePoolBody {
		return services.UpdatePoolBody{Egress: serverapi.NewOptEgressPolicy(serverapi.EgressPolicy{
			Allow: serverapi.NewOptNilStringArray(allow), DenyByDefault: serverapi.NewOptBool(true),
		})}
	}
	updated, err := svc.UpdatePool(ctx, "project-1", "pool-1", level("10.0.0.0/8"))
	if err != nil {
		t.Fatalf("update pool: %v", err)
	}
	if !updated.Egress.DenyByDefault || len(updated.Egress.Allow) != 1 || updated.Egress.Allow[0] != "10.0.0.0/8" {
		t.Fatalf("egress = %+v", updated.Egress)
	}
	if len(recorded.marked) != 1 || recorded.marked[0] != "project-1/pool-1/" {
		t.Fatalf("converged %v, want the pool's discoboxes", recorded.marked)
	}

	_, err = svc.UpdatePool(ctx, "project-1", "pool-1", level("10.0.0.0/33"))
	requireBadRequest(t, err, "10.0.0.0/33")
}
