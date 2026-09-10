package pools

import (
	"context"
	"testing"

	"aidanwoods.dev/go-paseto"
	"github.com/discobox-ai/discobox/judge"
	"github.com/discobox-ai/discobox/server/internal/auth"
	poolagentauth "github.com/discobox-ai/discobox/server/internal/auth/poolagent"
	"github.com/discobox-ai/discobox/server/internal/model"
)

func TestPoolJudgeDefaultsOverridesAndDedicatedIdentity(t *testing.T) {
	s, signer := newAgentServiceTestFixture(t)
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{Type: auth.PrincipalTypePool, PoolID: "pool-a"})
	for _, id := range []string{"harness-a", "harness-b"} {
		if err := s.store.CreateHarnessConfig(ctx, &model.HarnessConfig{ID: id, ProjectID: "project-1", Slug: id, Name: id, Configured: true, Image: "example/harness:latest"}); err != nil {
			t.Fatal(err)
		}
	}
	project, err := s.store.GetProject(ctx, "project-1")
	if err != nil {
		t.Fatal(err)
	}
	project.DefaultHarnessConfigID = "harness-a"
	if err := s.store.UpsertProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	first, err := s.GetPoolJudgeRuntime(ctx, "pool-a")
	if err != nil {
		t.Fatal(err)
	}
	if first.Harness.ID != "harness-a" || first.SandboxId == "sandbox-a" || first.Revision == "" {
		t.Fatalf("bad dedicated runtime: %s", first.SandboxId)
	}
	again, err := s.GetPoolJudgeRuntime(ctx, "pool-a")
	if err != nil {
		t.Fatal(err)
	}
	if first.SandboxId != again.SandboxId || first.Revision != again.Revision {
		t.Fatal("unchanged configuration replaced the judge")
	}
	key, err := signer.EnsureTrustKey(ctx)
	if err != nil {
		t.Fatal(err)
	}
	parser := paseto.NewParserForValidNow()
	parser.AddRule(paseto.ForAudience(poolagentauth.SandboxAgentAudience))
	token, err := parser.ParseV4Public(decodeTestPublicKey(t, key), first.Token, nil)
	if err != nil {
		t.Fatal(err)
	}
	var scopes []string
	_ = token.Get("scopes", &scopes)
	if len(scopes) != 1 || scopes[0] != judge.Scope {
		t.Fatalf("broad judge token: %v", scopes)
	}
	project.JudgeHarnessConfigID = "harness-b"
	if err := s.store.UpsertProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	override, err := s.GetPoolJudgeRuntime(ctx, "pool-a")
	if err != nil {
		t.Fatal(err)
	}
	if override.Harness.ID != "harness-b" || override.SandboxId == first.SandboxId {
		t.Fatal("override did not replace runtime")
	}
	if _, err := s.GetPoolJudgeRuntime(ctx, "pool-b"); err == nil {
		t.Fatal("cross-pool judge token issued")
	}
	if _, err := s.GetPoolJudgeRuntime(context.Background(), "pool-a"); err == nil {
		t.Fatal("anonymous judge token issued")
	}
}
