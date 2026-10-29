package buildkitagent

import (
	"context"
	"strings"
	"testing"

	controlapi "github.com/moby/buildkit/api/services/control"
	gatewayapi "github.com/moby/buildkit/frontend/gateway/pb"
	"github.com/moby/buildkit/solver/pb"
	"github.com/moby/buildkit/sourcepolicy"
	spb "github.com/moby/buildkit/sourcepolicy/pb"
)

// A build's result is pushed into the namespace of the sandbox that asked for
// it, named where buildkitd reaches the registry (ADR 26-10-29-160 §2).
func TestASolveExportsIntoItsSandboxsNamespace(t *testing.T) {
	req := &controlapi.SolveRequest{
		Exporters: []*controlapi.Exporter{
			{Type: "image", Attrs: map[string]string{"name": "registry.discobox.internal/sbx_mine/discobox-build/0123:build", "push": "true"}},
			{Type: "local", Attrs: map[string]string{"dest": "/out"}},
		},
		Cache: &controlapi.CacheOptions{
			Imports: []*controlapi.CacheOptionsEntry{{Type: "registry", Attrs: map[string]string{"ref": "registry.discobox.internal/sbx_mine/cache:main"}}},
			Exports: []*controlapi.CacheOptionsEntry{{Type: "registry", Attrs: map[string]string{"ref": "ghcr.io/example/cache:main"}}},
		},
	}
	if err := bindToNamespace(req, "sbx_mine"); err != nil {
		t.Fatal(err)
	}
	if got := req.Exporters[0].Attrs["name"]; got != "127.0.0.1:5000/sbx_mine/discobox-build/0123:build" {
		t.Errorf("image export name = %q", got)
	}
	if got := req.Cache.Imports[0].Attrs["ref"]; got != "127.0.0.1:5000/sbx_mine/cache:main" {
		t.Errorf("cache import ref = %q", got)
	}
	if got := req.Cache.Exports[0].Attrs["ref"]; got != "ghcr.io/example/cache:main" {
		t.Errorf("a cache at another registry was rewritten: %q", got)
	}
}

// No sandbox can have the builder write into another's namespace, or push an
// image anywhere but its own.
func TestASolveNamingAnotherNamespaceIsRefused(t *testing.T) {
	for name, req := range map[string]*controlapi.SolveRequest{
		"another sandbox's namespace":  {Exporters: []*controlapi.Exporter{{Type: "image", Attrs: map[string]string{"name": "registry.discobox.internal/sbx_theirs/x:1", "push": "true"}}}},
		"one of several names":         {Exporters: []*controlapi.Exporter{{Type: "image", Attrs: map[string]string{"name": "registry.discobox.internal/sbx_mine/x:1,registry.discobox.internal/sbx_theirs/x:1"}}}},
		"a prefix of the namespace":    {Exporters: []*controlapi.Exporter{{Type: "image", Attrs: map[string]string{"name": "registry.discobox.internal/sbx_mine_too/x:1"}}}},
		"the registry by its address":  {Exporters: []*controlapi.Exporter{{Type: "registry", Attrs: map[string]string{"name": "127.0.0.1:5000/sbx_theirs/x:1"}}}},
		"another registry":             {Exporters: []*controlapi.Exporter{{Type: "image", Attrs: map[string]string{"name": "docker.io/someone/x:1", "push": "true"}}}},
		"the deprecated exporter":      {ExporterDeprecated: "image", ExporterAttrsDeprecated: map[string]string{"name": "registry.discobox.internal/sbx_theirs/x:1"}},
		"another sandbox's cache":      {Cache: &controlapi.CacheOptions{Exports: []*controlapi.CacheOptionsEntry{{Type: "registry", Attrs: map[string]string{"ref": "registry.discobox.internal/sbx_theirs/cache"}}}}},
		"a deprecated cache import":    {Cache: &controlapi.CacheOptions{ImportRefsDeprecated: []string{"registry.discobox.internal/sbx_theirs/cache"}}},
		"a cache by the address":       {Cache: &controlapi.CacheOptions{Exports: []*controlapi.CacheOptionsEntry{{Type: "registry", Attrs: map[string]string{"ref": "127.0.0.1:5000/sbx_theirs/cache"}}}}},
		"its own cache by the address": {Cache: &controlapi.CacheOptions{Imports: []*controlapi.CacheOptionsEntry{{Type: "registry", Attrs: map[string]string{"ref": "127.0.0.1:5000/sbx_mine/cache"}}}}},
		"a frontend's cache import":    {FrontendAttrs: map[string]string{"cache-imports": `[{"Type":"registry","Attrs":{"ref":"127.0.0.1:5000/sbx_theirs/cache"}}]`}},
		"a frontend's cache-from":      {FrontendAttrs: map[string]string{"cache-from": "ghcr.io/x/y,registry.discobox.internal/sbx_theirs/cache"}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := bindToNamespace(req, "sbx_mine"); err == nil {
				t.Fatal("the solve was accepted")
			}
		})
	}
	if err := bindToNamespace(&controlapi.SolveRequest{}, "Not/An/ID"); err == nil {
		t.Fatal("a sandbox with no valid namespace was given a build")
	}
}

// A gateway solve, where buildx carries a dockerfile build's options, has its
// cache imports bound the same way.
func TestAGatewaySolvesCacheImportsAreBound(t *testing.T) {
	req := &gatewayapi.SolveRequest{
		FrontendOpt:  map[string]string{"cache-imports": `[{"Type":"registry","Attrs":{"ref":"registry.discobox.internal/sbx_mine/cache"}}]`},
		CacheImports: []*gatewayapi.CacheOptionsEntry{{Type: "registry", Attrs: map[string]string{"ref": "registry.discobox.internal/sbx_mine/cache"}}},
	}
	if err := bindGatewaySolve(req, "sbx_mine"); err != nil {
		t.Fatal(err)
	}
	if got := req.CacheImports[0].Attrs["ref"]; got != "127.0.0.1:5000/sbx_mine/cache" {
		t.Errorf("cache import = %q", got)
	}
	if got := req.FrontendOpt["cache-imports"]; !strings.Contains(got, `"ref":"127.0.0.1:5000/sbx_mine/cache"`) {
		t.Errorf("frontend cache-imports = %s", got)
	}
	for _, refused := range []*gatewayapi.SolveRequest{
		{CacheImports: []*gatewayapi.CacheOptionsEntry{{Type: "registry", Attrs: map[string]string{"ref": "127.0.0.1:5000/sbx_theirs/cache"}}}},
		{FrontendOpt: map[string]string{"cache-imports": `[{"Type":"registry","Attrs":{"ref":"registry.discobox.internal/sbx_theirs/cache"}}]`}},
	} {
		if err := bindGatewaySolve(refused, "sbx_mine"); err == nil {
			t.Errorf("a gateway solve importing another namespace's cache was accepted: %v", refused)
		}
	}
}

// An image a build reads from the sandbox's namespace — a local base the shim
// published (ADR 0047) — is fetched from where buildkitd reaches the registry,
// whatever rules the sandbox's own policy carries; another namespace's is not
// rewritten, so it is never fetched from the registry at all.
func TestASolveReadsItsNamespaceFromTheRegistrysAddress(t *testing.T) {
	own := &spb.Policy{Version: 1, Rules: []*spb.Rule{{
		Action:   spb.PolicyAction_ALLOW,
		Selector: &spb.Selector{Identifier: "docker-image://*", MatchType: spb.MatchType_WILDCARD},
	}}}
	ownRule := own.Rules[0]
	req := &controlapi.SolveRequest{SourcePolicy: own}
	if err := bindToNamespace(req, "sbx_mine"); err != nil {
		t.Fatal(err)
	}
	engine := sourcepolicy.NewEngine([]*spb.Policy{req.SourcePolicy})
	for in, want := range map[string]string{
		"docker-image://registry.discobox.internal/sbx_mine/base:local":   "docker-image://127.0.0.1:5000/sbx_mine/base:local",
		"docker-image://registry.discobox.internal/sbx_theirs/base:local": "docker-image://registry.discobox.internal/sbx_theirs/base:local",
		"docker-image://docker.io/library/debian:12":                      "docker-image://docker.io/library/debian:12",
		"docker-image://127.0.0.1:5000/sbx_mine/base:local":               "docker-image://127.0.0.1:5000/sbx_mine/base:local",
	} {
		op := &pb.SourceOp{Identifier: in}
		if _, err := engine.Evaluate(context.Background(), op); err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if op.Identifier != want {
			t.Errorf("%s is fetched as %s, want %s", in, op.Identifier, want)
		}
	}
	// Another namespace at the registry's address is denied, whatever the
	// sandbox's own policy allowed.
	for _, refused := range []string{
		"docker-image://127.0.0.1:5000/sbx_theirs/base:local",
		"docker-image://127.0.0.1:5000/sbx_mine_too/base:local",
	} {
		if _, err := engine.Evaluate(context.Background(), &pb.SourceOp{Identifier: refused}); err == nil {
			t.Errorf("%s was allowed", refused)
		}
	}
	kept := false
	for _, rule := range req.SourcePolicy.Rules {
		kept = kept || rule == ownRule
	}
	if !kept {
		t.Error("the sandbox's own rules were dropped")
	}
}
