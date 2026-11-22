package handlers

import (
	"context"
	"testing"
	"time"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/pkg/poolauth"
	"github.com/discobox-ai/discobox/server/internal/auth"
	svcapi "github.com/discobox-ai/discobox/server/internal/services"
)

// driverKeyPools answers every pool's driver key, and records which pool
// asked.
type driverKeyPools struct {
	svcapi.PoolService
	asked []string
}

func (p *driverKeyPools) ResolveDriverKey(_ context.Context, poolID, sentinel, host string) (string, error) {
	p.asked = append(p.asked, poolID+" "+sentinel+" "+host)
	return "key-REAL", nil
}

// The driver key is answered to a pool holding the resolve scope, for the
// pool its assertion names (never one a body could name), and to nothing
// else (ADR 26-11-11-080 §9).
func TestResolveDriverKeyIsAnsweredToTheAskingPool(t *testing.T) {
	pools := &driverKeyPools{}
	h := New(svcapi.Services{Pools: pools})
	body := &serverapi.ResolveDriverKeyBody{Sentinel: "key-SENTINEL", Host: "control.example.com"}
	params := serverapi.ResolveDriverKeyParams{PoolId: "pool-other"}
	for name, ctx := range map[string]context.Context{
		"no principal": context.Background(),
		"a user":       auth.WithPrincipal(context.Background(), auth.Principal{Type: auth.PrincipalTypeUser, UserID: "user-1"}),
		"unscoped":     auth.WithPrincipal(context.Background(), auth.Principal{Type: auth.PrincipalTypePool, PoolID: "pool-1"}),
	} {
		res, err := h.ResolveDriverKey(ctx, body, params)
		if _, answered := res.(*serverapi.ResolveDriverKeyResponse); err != nil || answered {
			t.Errorf("%s: ResolveDriverKey() = %T, %v; want it refused", name, res, err)
		}
	}
	if len(pools.asked) != 0 {
		t.Fatalf("the service was asked %q before any pool was authorized", pools.asked)
	}
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{Type: auth.PrincipalTypePool, PoolID: "pool-1", Scopes: []string{poolauth.ScopeSecretResolve}})
	res, err := h.ResolveDriverKey(ctx, body, params)
	got, ok := res.(*serverapi.ResolveDriverKeyResponse)
	if err != nil || !ok || got.Value != "key-REAL" {
		t.Fatalf("ResolveDriverKey() = %#v, %v; want the key", res, err)
	}
	// An answer with no expiry is one the proxy holds for no call at all.
	if expires, set := got.ExpiresAt.Get(); !set || !expires.After(time.Now()) {
		t.Fatalf("ResolveDriverKey() expires %v (set %v), want a time the proxy holds the key until", expires, set)
	}
	if len(pools.asked) != 1 || pools.asked[0] != "pool-1 key-SENTINEL control.example.com" {
		t.Fatalf("the service was asked %q, want the asking pool's", pools.asked)
	}
}
