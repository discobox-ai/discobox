package pools

import (
	"context"
	"strings"
	"testing"

	apigen "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/pkg/pooloutbound"
	"github.com/discobox-ai/discobox/server/internal/services"
)

func TestPoolOverrideUsesOneObjectAndNullRevealsYAML(t *testing.T) {
	s := newSizeFixture(t)
	s.pools = NewControlPlane(s.store, nil)
	s.pools.OutboundDefaults = pooloutbound.Defaults{Configured: true, Global: pooloutbound.Resolved{HTTP: "http://global:1", HTTPS: "http://global:2"}, GlobalMode: "explicit", ByPool: map[string]pooloutbound.Resolved{}}
	ctx := context.Background()
	proxy := apigen.PoolProxy{Mode: apigen.NewOptPoolProxyMode(apigen.PoolProxyModeExplicit), HTTP: apigen.NewOptString("http://api:1")}
	pool, err := s.CreatePool(ctx, "project-1", services.CreatePoolBody{Name: "outbound", ProviderInstanceId: "prov-unsized", Proxy: apigen.NewOptNilPoolProxy(proxy)})
	if err != nil {
		t.Fatal(err)
	}
	s.pools.OutboundDefaults.ByPool[pool.ID] = pooloutbound.Resolved{HTTP: "http://yaml:1", HTTPS: "http://yaml:2"}
	got, err := s.GetPool(ctx, "project-1", pool.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Outbound.HTTP != "http://api:1" || got.Outbound.HTTPS != "" || !got.OutboundStatus.ShadowsYAML {
		t.Fatalf("API override merged or lost: %+v", got.OutboundStatus)
	}
	if _, err := s.UpdatePool(ctx, "project-1", pool.ID, services.UpdatePoolBody{Name: apigen.NewOptString("renamed")}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetPool(ctx, "project-1", pool.ID)
	if got.Outbound.HTTP != "http://api:1" {
		t.Fatal("omission removed API override")
	}
	removed := apigen.OptNilPoolProxy{}
	removed.SetToNull()
	got, err = s.UpdatePool(ctx, "project-1", pool.ID, services.UpdatePoolBody{Proxy: removed})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outbound.HTTP != "http://yaml:1" || got.Outbound.HTTPS != "http://yaml:2" || got.OutboundStatus.Source != "yaml" {
		t.Fatalf("null did not reveal YAML: %+v", got.OutboundStatus)
	}
	direct := apigen.PoolProxy{Mode: apigen.NewOptPoolProxyMode(apigen.PoolProxyModeDirect)}
	got, err = s.UpdatePool(ctx, "project-1", pool.ID, services.UpdatePoolBody{Proxy: apigen.NewOptNilPoolProxy(direct)})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outbound.HTTP != "" || got.Outbound.HTTPS != "" {
		t.Fatal("pool direct inherited a proxy")
	}
}

func TestRedactedCredentialsArePreservedAndExplicitlyRemoved(t *testing.T) {
	//nolint:gosec // Synthetic fixture verifies proxy credential handling.
	old := &pooloutbound.Intent{Mode: "explicit", HTTP: "http://user:private@proxy:3128"}
	input := apigen.PoolProxy{Mode: apigen.NewOptPoolProxyMode(apigen.PoolProxyModeExplicit), HTTP: apigen.NewOptString("http://proxy:3128")}
	got, err := proxyWrite(input, old)
	if err != nil {
		t.Fatal(err)
	}
	if got.HTTP != old.HTTP {
		t.Fatal("redacted round trip cleared credentials")
	}
	input.ClearHttpCredentials = apigen.NewOptBool(true)
	got, err = proxyWrite(input, old)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got.HTTP, "private") {
		t.Fatal("explicit removal kept credentials")
	}
	input.HTTP = apigen.NewOptString("http://new:replacement@proxy:3128")
	if _, err := proxyWrite(input, old); err == nil {
		t.Fatal("replacement and clear were accepted together")
	}
}
