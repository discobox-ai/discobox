package pools

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/discobox-ai/discobox/server/internal/model"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
)

// driverKeyPoolProvider answers the sentinel of each pool's host from a
// table, and records what it was asked.
type driverKeyPoolProvider struct {
	*auditPoolProvider
	keys  map[string]string
	asked []string
}

func (p *driverKeyPoolProvider) ResolveDriverKey(_ context.Context, _ *model.SandboxProviderInstance, pool *model.Pool, sentinel, host string) (string, error) {
	p.asked = append(p.asked, pool.ID+" "+sentinel+" "+host)
	value, ok := p.keys[pool.ID]
	if !ok {
		return "", fmt.Errorf("%w: no sentinel here", sandbox.ErrDriverKeyRefused)
	}
	return value, nil
}

// A pool's proxy is answered for the asking pool's own provider: the runtime
// is asked for that pool, its sentinel and the host. A runtime that refuses
// is a 403, as is a pool being deleted, whose runtime is never asked; an
// unknown pool is a 404 (ADR 26-11-11-080 §9).
func TestResolveDriverKeyIsThePoolsOwn(t *testing.T) {
	svc, audit := newAuditServiceWith(t, func(pool *model.Pool) {
		if pool.ID == "pool-c" {
			pool.DesiredState = model.DesiredStateDeleted
		}
	})
	provider := &driverKeyPoolProvider{auditPoolProvider: audit, keys: map[string]string{"pool-a": "key-REAL", "pool-c": "key-REAL"}}
	svc.providers.RegisterProvider("audit", provider)
	ctx := context.Background()

	value, err := svc.ResolveDriverKey(ctx, "pool-a", "key-SENTINEL", "control.example.com")
	if err != nil || value != "key-REAL" {
		t.Fatalf("ResolveDriverKey(pool-a) = %q, %v; want the provider's key", value, err)
	}
	if len(provider.asked) != 1 || provider.asked[0] != "pool-a key-SENTINEL control.example.com" {
		t.Fatalf("the runtime was asked %q, want pool-a's sentinel at the host", provider.asked)
	}
	for _, tc := range []struct {
		pool   string
		status int
	}{
		{"pool-b", http.StatusForbidden},
		{"pool-c", http.StatusForbidden},
		{"pool-z", http.StatusNotFound},
	} {
		if value, err := svc.ResolveDriverKey(ctx, tc.pool, "key-SENTINEL", "control.example.com"); statusOf(err) != tc.status || value != "" {
			t.Errorf("ResolveDriverKey(%s) = %q, %v; want %d", tc.pool, value, err, tc.status)
		}
	}
	if len(provider.asked) != 2 {
		t.Fatalf("the runtime was asked %q; want it never asked for a pool being deleted", provider.asked)
	}
}
