package pools

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/internal/services"
)

func (p *auditPoolProvider) OpenPprof(_ context.Context, pool *model.Pool, process, profile string, seconds int) (*sandbox.PoolProfile, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.errs[pool.ID]; err != nil {
		return nil, err
	}
	if process == "mediator" {
		// The agent's own refusal, as poolruntime reads it.
		return nil, apperrors.NewStatusError(http.StatusBadGateway, "the pool's mediator does not answer")
	}
	return &sandbox.PoolProfile{Body: io.NopCloser(strings.NewReader(pool.ID + " " + process + " " + profile)), ContentType: "application/octet-stream"}, nil
}

// A profile is fetched from the named pool's runtime only when the pool has
// profiling on, and every refusal says which pool and why.
func TestOpenPoolPprof(t *testing.T) {
	svc, _ := newAuditServiceWith(t, func(pool *model.Pool) {
		pool.Profiling = pool.ID != "pool-b"
	})
	ctx := context.Background()
	profiled, err := svc.OpenPoolPprof(ctx, "project-1", "pool-a", "proxy", "heap", 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	body, _ := io.ReadAll(profiled.Body)
	_ = profiled.Body.Close()
	if string(body) != "pool-a proxy heap" {
		t.Fatalf("profile = %q, want pool-a's proxy heap", body)
	}

	for _, tc := range []struct {
		pool, process string
		status        int
		want          string
	}{
		{"pool-b", "agent", http.StatusNotFound, "profiling is off for pool pool-b"},
		{"pool-c", "agent", http.StatusServiceUnavailable, "predates profiling"},
		{"pool-a", "mediator", http.StatusBadGateway, "does not answer"},
		{"pool-z", "agent", http.StatusNotFound, "pool not found"},
	} {
		_, err := svc.OpenPoolPprof(ctx, "project-1", tc.pool, tc.process, "heap", 0)
		if status := statusOf(err); status != tc.status || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("profile %s's %s = %v (status %d), want %d saying %q", tc.pool, tc.process, err, status, tc.status, tc.want)
		}
	}
}

// Profiling is off unless a pool asks for it, at create or later, and is
// stored with the pool (ADR 26-11-24-418 §2).
func TestAPoolIsProfiledOnlyWhenItAsks(t *testing.T) {
	svc := newSizeFixture(t)
	ctx := context.Background()

	byDefault, err := svc.CreatePool(ctx, "project-1", services.CreatePoolBody{Name: "default", ProviderInstanceId: "prov-unsized"})
	if err != nil {
		t.Fatalf("create pool: %v", err)
	}
	if byDefault.Profiling {
		t.Fatal("a pool made with nothing said is profiled")
	}
	if _, err := svc.UpdatePool(ctx, "project-1", byDefault.ID, services.UpdatePoolBody{Profiling: serverapi.NewOptBool(true)}); err != nil {
		t.Fatalf("update pool: %v", err)
	}
	stored, err := svc.store.GetPool(ctx, "project-1", byDefault.ID)
	if err != nil || !stored.Profiling {
		t.Fatalf("stored pool = %+v, %v; want profiling on after the update", stored, err)
	}
	if _, err := svc.UpdatePool(ctx, "project-1", byDefault.ID, services.UpdatePoolBody{Name: serverapi.NewOptString("renamed")}); err != nil {
		t.Fatalf("update pool: %v", err)
	}
	if stored, err = svc.store.GetPool(ctx, "project-1", byDefault.ID); err != nil || !stored.Profiling {
		t.Fatalf("an update that said nothing of profiling changed it: %+v, %v", stored, err)
	}

	asked, err := svc.CreatePool(ctx, "project-1", services.CreatePoolBody{Name: "profiled", ProviderInstanceId: "prov-unsized", Profiling: serverapi.NewOptBool(true)})
	if err != nil {
		t.Fatalf("create pool: %v", err)
	}
	if stored, err = svc.store.GetPool(ctx, "project-1", asked.ID); err != nil || !stored.Profiling {
		t.Fatalf("a pool made asking for profiling is stored without it: %+v, %v", stored, err)
	}
}
