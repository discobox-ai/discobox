package pools

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"aidanwoods.dev/go-paseto"
	"github.com/go-faster/jx"

	serverapi "github.com/obot-platform/discobox/api/gen"
	"github.com/obot-platform/discobox/server/internal/auth"
	poolagentauth "github.com/obot-platform/discobox/server/internal/auth/poolagent"
	"github.com/obot-platform/discobox/server/internal/database"
	"github.com/obot-platform/discobox/server/internal/model"
	"github.com/obot-platform/discobox/server/internal/secrets"
	services "github.com/obot-platform/discobox/server/internal/services"
	"github.com/obot-platform/discobox/server/internal/store"
)

func newAgentServiceTestFixture(t *testing.T) (*Service, *poolagentauth.Manager) {
	t.Helper()
	ctx := context.Background()
	db, err := database.New(database.Config{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate db: %v", err)
	}
	appStore := store.New(db.Write, db.Read)

	key, err := secrets.GenerateBase64Key()
	if err != nil {
		t.Fatalf("generate sealer key: %v", err)
	}
	sealer, err := secrets.NewAESGCMSealerFromBase64Key(key)
	if err != nil {
		t.Fatalf("new sealer: %v", err)
	}
	agentAuth := poolagentauth.NewManager(appStore, sealer)

	controlPlane := NewControlPlane(appStore, nil)
	controlPlane.SetAgentAuthManager(agentAuth)

	if err := appStore.UpsertProject(ctx, &model.Project{ID: "project-1", OwnerUserID: "user-1", Name: "project-1"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	for _, poolID := range []string{"pool-a", "pool-b"} {
		providerID := "prov-" + poolID
		if err := appStore.CreateSandboxProviderInstance(ctx, &model.SandboxProviderInstance{ID: providerID, ProjectID: "project-1", Type: "docker", Name: providerID}); err != nil {
			t.Fatalf("create provider %s: %v", providerID, err)
		}
		if err := appStore.CreatePool(ctx, &model.Pool{ID: poolID, ProjectID: "project-1", PoolManifest: model.PoolManifest{Name: poolID, ProviderInstanceID: providerID}}); err != nil {
			t.Fatalf("create pool %s: %v", poolID, err)
		}
	}
	if err := appStore.CreateSandbox(ctx, &model.Sandbox{
		ID: "sandbox-a", ProjectID: "project-1", PoolID: "pool-a", CreatedByUserID: "user-1", Name: "sandbox-a",
	}); err != nil {
		t.Fatalf("create sandbox-a: %v", err)
	}
	if err := appStore.CreateSandbox(ctx, &model.Sandbox{
		ID: "sandbox-b", ProjectID: "project-1", PoolID: "pool-b", CreatedByUserID: "user-1", Name: "sandbox-b",
	}); err != nil {
		t.Fatalf("create sandbox-b: %v", err)
	}

	return NewService(appStore, controlPlane), agentAuth
}

// TestMintSandboxAgentStatusTokensScopeIsAlwaysStatusRead is the load-bearing
// test for this endpoint's whole reason to exist: no matter what a caller
// requests, the minted token must carry exactly the status:read scope and
// nothing else, since a pool agent is meant to be able to read a sandbox's
// status and nothing more.
func TestMintSandboxAgentStatusTokensScopeIsAlwaysStatusRead(t *testing.T) {
	svc, agentAuth := newAgentServiceTestFixture(t)
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{
		Type:   auth.PrincipalTypePool,
		PoolID: "pool-a",
	})

	resp, err := svc.MintSandboxAgentStatusTokens(ctx, "pool-a", services.MintSandboxAgentStatusTokensBody{
		SandboxIds: []string{"sandbox-a"},
	})
	if err != nil {
		t.Fatalf("mint tokens: %v", err)
	}
	if len(resp.Tokens) != 1 {
		t.Fatalf("tokens = %d, want 1", len(resp.Tokens))
	}
	tok := resp.Tokens[0]
	if tok.SandboxId != "sandbox-a" {
		t.Fatalf("sandboxId = %q, want sandbox-a", tok.SandboxId)
	}

	publicKeyText, err := agentAuth.EnsureTrustKey(ctx)
	if err != nil {
		t.Fatalf("ensure trust key: %v", err)
	}
	publicKey := decodeTestPublicKey(t, publicKeyText)
	parser := paseto.NewParserForValidNow()
	parser.AddRule(paseto.ForAudience(poolagentauth.SandboxAgentAudience))
	parsed, err := parser.ParseV4Public(publicKey, tok.Token, nil)
	if err != nil {
		t.Fatalf("parse minted token: %v", err)
	}
	var scopes []string
	if err := parsed.Get("scopes", &scopes); err != nil {
		t.Fatalf("read scopes claim: %v", err)
	}
	if len(scopes) != 1 || scopes[0] != poolagentauth.ScopeStatusRead {
		t.Fatalf("scopes = %v, want exactly [%q]", scopes, poolagentauth.ScopeStatusRead)
	}
}

// TestMintSandboxAgentStatusTokensSkipsSandboxesOutsidePool confirms a pool
// cannot obtain a status token for a sandbox it does not host, and that one
// such request does not fail the whole batch.
func TestMintSandboxAgentStatusTokensSkipsSandboxesOutsidePool(t *testing.T) {
	svc, _ := newAgentServiceTestFixture(t)
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{
		Type:   auth.PrincipalTypePool,
		PoolID: "pool-a",
	})

	resp, err := svc.MintSandboxAgentStatusTokens(ctx, "pool-a", services.MintSandboxAgentStatusTokensBody{
		SandboxIds: []string{"sandbox-a", "sandbox-b", "does-not-exist"},
	})
	if err != nil {
		t.Fatalf("mint tokens: %v", err)
	}
	if len(resp.Tokens) != 1 || resp.Tokens[0].SandboxId != "sandbox-a" {
		t.Fatalf("tokens = %+v, want exactly sandbox-a", resp.Tokens)
	}
}

// TestReportSandboxAgentStatusWritesOwnedSandbox confirms a pool's status
// report lands on a sandbox it hosts.
func TestReportSandboxAgentStatusWritesOwnedSandbox(t *testing.T) {
	svc, _ := newAgentServiceTestFixture(t)
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{
		Type:   auth.PrincipalTypePool,
		PoolID: "pool-a",
	})
	observedAt := time.Now().UTC().Truncate(time.Second)

	err := svc.ReportSandboxAgentStatus(ctx, "pool-a", services.ReportSandboxAgentStatusBody{
		Sandboxes: []serverapi.SandboxAgentStatusEntry{
			{
				SandboxId:  "sandbox-a",
				Status:     serverapi.SandboxAgentStatusEntryStatus{"sessions": jx.Raw("[]")},
				ObservedAt: observedAt,
			},
		},
	})
	if err != nil {
		t.Fatalf("report status: %v", err)
	}

	got, err := svc.store.GetSandbox(ctx, "project-1", "sandbox-a")
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if got.AgentStatusObservedAt == nil || !got.AgentStatusObservedAt.Equal(observedAt) {
		t.Fatalf("agentStatusObservedAt = %v, want %v", got.AgentStatusObservedAt, observedAt)
	}
}

// TestReportSandboxAgentStatusSkipsSandboxOutsidePool confirms a pool cannot
// write status onto a sandbox it does not host, and that this does not error
// the whole request (the entry is silently skipped).
func TestReportSandboxAgentStatusSkipsSandboxOutsidePool(t *testing.T) {
	svc, _ := newAgentServiceTestFixture(t)
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{
		Type:   auth.PrincipalTypePool,
		PoolID: "pool-a",
	})
	observedAt := time.Now().UTC().Truncate(time.Second)

	err := svc.ReportSandboxAgentStatus(ctx, "pool-a", services.ReportSandboxAgentStatusBody{
		Sandboxes: []serverapi.SandboxAgentStatusEntry{
			{SandboxId: "sandbox-b", Status: serverapi.SandboxAgentStatusEntryStatus{}, ObservedAt: observedAt},
		},
	})
	if err != nil {
		t.Fatalf("report status: %v", err)
	}

	got, err := svc.store.GetSandbox(ctx, "project-1", "sandbox-b")
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if got.AgentStatusObservedAt != nil {
		t.Fatalf("agentStatusObservedAt = %v, want nil (pool-a must not write onto pool-b's sandbox)", got.AgentStatusObservedAt)
	}
}

// TestReportSandboxAgentStatusRequiresPoolPrincipal confirms a user principal
// (or none) cannot report status — only an authenticated pool agent can.
func TestReportSandboxAgentStatusRequiresPoolPrincipal(t *testing.T) {
	svc, _ := newAgentServiceTestFixture(t)
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{
		Type:   auth.PrincipalTypeUser,
		UserID: "user-1",
	})

	err := svc.ReportSandboxAgentStatus(ctx, "pool-a", services.ReportSandboxAgentStatusBody{})
	var statusErr interface{ StatusCode() int }
	if err == nil || !errors.As(err, &statusErr) || statusErr.StatusCode() != 403 {
		t.Fatalf("report status = %v, want 403 forbidden", err)
	}
}

func decodeTestPublicKey(t *testing.T, text string) paseto.V4AsymmetricPublicKey {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(text)
	if err != nil {
		t.Fatalf("decode public key: %v", err)
	}
	key, err := paseto.NewV4AsymmetricPublicKeyFromEd25519(ed25519.PublicKey(raw))
	if err != nil {
		t.Fatalf("load public key: %v", err)
	}
	return key
}

// reportedLastAccess is the newest client access the payload carries: the max
// of the sessions' lastAccessedAt, taken as observedAt when a client is
// attached at observation, and nil when the payload says nothing about access.
func TestReportedLastAccess(t *testing.T) {
	observed := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	access := observed.Add(-10 * time.Minute)
	older := observed.Add(-2 * time.Hour)

	sessions := func(body string) map[string]jx.Raw {
		return map[string]jx.Raw{"sessions": jx.Raw(body)}
	}

	if got := reportedLastAccess(map[string]jx.Raw{}, observed); got != nil {
		t.Fatalf("no sessions: %v, want nil", got)
	}
	if got := reportedLastAccess(sessions(`[]`), observed); got != nil {
		t.Fatalf("empty sessions: %v, want nil", got)
	}
	if got := reportedLastAccess(sessions(`[{"terminalId":"t1","primary":true,"state":"running","attacherCount":0,"execStatus":"running"}]`), observed); got != nil {
		t.Fatalf("never accessed: %v, want nil", got)
	}
	got := reportedLastAccess(sessions(
		`[{"terminalId":"t1","primary":true,"state":"running","attacherCount":0,"execStatus":"running","lastAccessedAt":"`+older.Format(time.RFC3339)+`"},`+
			`{"terminalId":"t2","primary":false,"state":"running","attacherCount":0,"execStatus":"running","lastAccessedAt":"`+access.Format(time.RFC3339)+`"}]`), observed)
	if got == nil || !got.Equal(access) {
		t.Fatalf("max of session access = %v, want %v", got, access)
	}
	// A client attached at observation is access at observation.
	got = reportedLastAccess(sessions(
		`[{"terminalId":"t1","primary":true,"state":"running","attacherCount":1,"execStatus":"running","lastAccessedAt":"`+older.Format(time.RFC3339)+`"}]`), observed)
	if got == nil || !got.Equal(observed) {
		t.Fatalf("attached now = %v, want %v", got, observed)
	}
}
