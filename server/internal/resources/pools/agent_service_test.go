package pools

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"aidanwoods.dev/go-paseto"
	"github.com/go-faster/jx"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/server/internal/auth"
	poolagentauth "github.com/discobox-ai/discobox/server/internal/auth/poolagent"
	"github.com/discobox-ai/discobox/server/internal/database"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/secrets"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
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

	return NewService(appStore, nil, controlPlane), agentAuth
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

func TestReportPoolResourcesSplitsAcrossPoolAndSandboxRows(t *testing.T) {
	svc, _ := newAgentServiceTestFixture(t)
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{
		Type:   auth.PrincipalTypePool,
		PoolID: "pool-a",
	})
	reportedAt := time.Now().UTC().Truncate(time.Second)
	observedAt := reportedAt.Add(-3 * time.Second)

	err := svc.ReportPoolResources(ctx, "pool-a", services.ReportPoolResourcesBody{
		Report: serverapi.PoolResourceReport{
			ReportedAt: reportedAt,
			CPU: serverapi.PoolCPUUsage{
				UsageUsec: 900_000_000,
				Vcpus:     serverapi.NewOptFloat64(8.1),
			},
			Memory:  serverapi.PoolMemoryUsage{CurrentBytes: 24 << 30},
			Storage: serverapi.PoolStorageUsage{Root: "/var/lib/discobox"},
		},
		Sandboxes: []serverapi.SandboxResourceConsumption{{
			SandboxId:  "sandbox-a",
			ObservedAt: serverapi.NewOptDateTime(observedAt),
			CPU: serverapi.NewOptSandboxCPUConsumption(serverapi.SandboxCPUConsumption{
				UsageUsec: 8_204_113_000,
				Vcpus:     serverapi.NewOptFloat64(3.71),
			}),
		}},
	})
	if err != nil {
		t.Fatalf("report resources: %v", err)
	}

	pool, err := svc.store.GetPoolByID(ctx, "pool-a")
	if err != nil {
		t.Fatalf("get pool: %v", err)
	}
	if pool.ResourcesReportedAt == nil || !pool.ResourcesReportedAt.Equal(reportedAt) {
		t.Fatalf("resourcesReportedAt = %v, want %v", pool.ResourcesReportedAt, reportedAt)
	}
	// The pool row carries the pool-wide half and deliberately not the
	// per-sandbox array, which lives on the sandbox rows.
	if !strings.Contains(string(pool.Resources), `"root":"/var/lib/discobox"`) {
		t.Errorf("pool resources missing pool-wide storage: %s", pool.Resources)
	}
	if strings.Contains(string(pool.Resources), "sandbox-a") {
		t.Errorf("pool resources duplicated the per-sandbox array: %s", pool.Resources)
	}

	sandbox, err := svc.store.GetSandbox(ctx, "project-1", "sandbox-a")
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if sandbox.ResourcesObservedAt == nil || !sandbox.ResourcesObservedAt.Equal(observedAt) {
		t.Fatalf("resourcesObservedAt = %v, want the sandbox's own observation %v",
			sandbox.ResourcesObservedAt, observedAt)
	}
	if !strings.Contains(string(sandbox.Resources), `"vcpus":3.71`) {
		t.Errorf("sandbox resources missing its rate: %s", sandbox.Resources)
	}
	// Resource consumption is not evidence a person touched the sandbox: a
	// build burns CPU with nobody watching.
	if sandbox.LastActiveAt != nil {
		t.Errorf("lastActiveAt = %v, want nil (CPU use is not client activity)", sandbox.LastActiveAt)
	}
}

// The stored blobs must survive the API projection. Both schemas forbid
// additional properties, so a stored shape that does not match the contract
// fails the read rather than the write — which would surface as `pool get`
// breaking only once a pool had actually reported.
func TestReportedResourcesSurviveTheAPIProjection(t *testing.T) {
	svc, _ := newAgentServiceTestFixture(t)
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{
		Type:   auth.PrincipalTypePool,
		PoolID: "pool-a",
	})
	reportedAt := time.Now().UTC().Truncate(time.Second)

	err := svc.ReportPoolResources(ctx, "pool-a", services.ReportPoolResourcesBody{
		Report: serverapi.PoolResourceReport{
			ReportedAt: reportedAt,
			CPU: serverapi.PoolCPUUsage{
				UsageUsec: 900_000_000, UserUsec: 700_000_000, SystemUsec: 200_000_000,
				Vcpus: serverapi.NewOptFloat64(8.1), CapacityVcpus: serverapi.NewOptFloat64(16),
			},
			Memory: serverapi.PoolMemoryUsage{CurrentBytes: 24 << 30},
			Storage: serverapi.PoolStorageUsage{
				Root:       "/var/lib/discobox",
				Filesystem: serverapi.PoolFilesystemUsage{TotalBytes: 500 << 30, UsedBytes: 120 << 30, FreeBytes: 380 << 30},
				// The walked half is stamped with its own schedule, because it
				// is deliberately older than the report that carries it.
				Walk: serverapi.NewOptPoolStorageWalk(serverapi.PoolStorageWalk{
					ObservedAt:      reportedAt.Add(-8 * time.Minute),
					DurationMillis:  11400,
					IntervalSeconds: 570,
					NextScanAt:      reportedAt.Add(90 * time.Second),
					CacheBytes:      41 << 30,
					BuildBytes:      9 << 30,
				}),
			},
		},
		Sandboxes: []serverapi.SandboxResourceConsumption{{
			SandboxId: "sandbox-a",
			CPU: serverapi.NewOptSandboxCPUConsumption(serverapi.SandboxCPUConsumption{
				UsageUsec: 8_204_113_000, Vcpus: serverapi.NewOptFloat64(3.71),
			}),
			Memory: serverapi.NewOptSandboxMemoryConsumption(serverapi.SandboxMemoryConsumption{
				CurrentBytes: 6 << 30, VirtualBytes: 12 << 30, ResidentBytes: 5 << 30,
			}),
		}},
	})
	if err != nil {
		t.Fatalf("report resources: %v", err)
	}

	poolModel, err := svc.store.GetPoolByID(ctx, "pool-a")
	if err != nil {
		t.Fatalf("get pool: %v", err)
	}
	apiPool, err := services.Convert[serverapi.Pool](poolModel)
	if err != nil {
		t.Fatalf("project pool through the API schema: %v\nstored: %s", err, poolModel.Resources)
	}
	// The blob rides an open object so an unreadable one can never fail the
	// pool's own read; a consumer decodes it into the documented shape.
	report, ok := decodeOpaqueForTest[serverapi.PoolResourceReport](t, apiPool.Resources)
	if !ok {
		t.Fatal("pool.resources did not survive the projection")
	}
	if report.CPU.Vcpus.Or(0) != 8.1 {
		t.Errorf("pool vcpus = %v, want 8.1", report.CPU.Vcpus.Or(0))
	}
	if report.Storage.Filesystem.FreeBytes != 380<<30 {
		t.Errorf("pool filesystem = %+v", report.Storage.Filesystem)
	}
	walk, ok := report.Storage.Walk.Get()
	if !ok {
		t.Fatal("the walked attribution did not survive the projection")
	}
	if walk.CacheBytes != 41<<30 || walk.BuildBytes != 9<<30 {
		t.Errorf("walked totals = %+v", walk)
	}
	// Without the schedule a reader cannot tell a fresh figure from an hour-old
	// one, which is the whole basis for caching the sweep at all.
	if walk.DurationMillis != 11400 || walk.IntervalSeconds != 570 || walk.NextScanAt.IsZero() {
		t.Errorf("walk schedule = %+v", walk)
	}

	sandboxModel, err := svc.store.GetSandbox(ctx, "project-1", "sandbox-a")
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	apiSandbox, err := services.SandboxToAPI(sandboxModel, nil)
	if err != nil {
		t.Fatalf("project sandbox through the API schema: %v\nstored: %s", err, sandboxModel.Resources)
	}
	consumption, ok := decodeOpaqueForTest[serverapi.SandboxResourceConsumption](t, apiSandbox.Runtime.Resources)
	if !ok {
		t.Fatal("sandbox runtime.resources did not survive the projection")
	}
	cpu, ok := consumption.CPU.Get()
	if !ok || cpu.Vcpus.Or(0) != 3.71 {
		t.Errorf("sandbox cpu = %+v (present=%v)", cpu, ok)
	}
	memory, ok := consumption.Memory.Get()
	if !ok || memory.ResidentBytes != 5<<30 {
		t.Errorf("sandbox memory = %+v (present=%v)", memory, ok)
	}
}

// A blob this build cannot read must cost its own field, never the read of the
// resource carrying it — the failure a strictly-typed field caused in practice,
// where one stale pool row 500'd every pool listing.
func TestAPoolResourceBlobFromAnotherVersionDoesNotFailThePoolRead(t *testing.T) {
	svc, _ := newAgentServiceTestFixture(t)
	ctx := context.Background()
	// A shape no current schema describes, as an older or newer agent might
	// have written it.
	stale := json.RawMessage(`{"reportedAt":"2026-01-01T00:00:00Z","somethingElse":{"gone":1}}`)
	if err := svc.store.RecordPoolResources(ctx, "pool-a", stale, time.Now().UTC()); err != nil {
		t.Fatalf("record stale resources: %v", err)
	}

	poolModel, err := svc.store.GetPoolByID(ctx, "pool-a")
	if err != nil {
		t.Fatalf("get pool: %v", err)
	}
	apiPool, err := services.Convert[serverapi.Pool](poolModel)
	if err != nil {
		t.Fatalf("an unreadable resource blob failed the whole pool read: %v", err)
	}
	if apiPool.ID != "pool-a" {
		t.Errorf("pool = %+v", apiPool)
	}
}

func decodeOpaqueForTest[T any, PT interface {
	*T
	UnmarshalJSON([]byte) error
}, O interface{ Get() (M, bool) }, M ~map[string]jx.Raw](t *testing.T, opt O) (T, bool) {
	t.Helper()
	var out T
	raw, present := opt.Get()
	if !present || len(raw) == 0 {
		return out, false
	}
	// Rebuilt with jx, not encoding/json: a jx.Raw is a bare []byte with no
	// MarshalJSON, so encoding/json would base64-encode each field.
	var encoder jx.Encoder
	encoder.ObjStart()
	for field, value := range raw {
		encoder.FieldStart(field)
		encoder.Raw(value)
	}
	encoder.ObjEnd()
	if err := PT(&out).UnmarshalJSON(encoder.Bytes()); err != nil {
		return out, false
	}
	return out, true
}

// A pool must not write accounting onto a sandbox it does not host, and one// A pool must not write accounting onto a sandbox it does not host, and one
// such entry must not discard every other sandbox's figures for this tick.
func TestReportPoolResourcesSkipsSandboxOutsidePool(t *testing.T) {
	svc, _ := newAgentServiceTestFixture(t)
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{
		Type:   auth.PrincipalTypePool,
		PoolID: "pool-a",
	})

	err := svc.ReportPoolResources(ctx, "pool-a", services.ReportPoolResourcesBody{
		Report: serverapi.PoolResourceReport{ReportedAt: time.Now().UTC()},
		Sandboxes: []serverapi.SandboxResourceConsumption{
			{SandboxId: "sandbox-b"},
			{SandboxId: "sandbox-a"},
		},
	})
	if err != nil {
		t.Fatalf("report resources: %v", err)
	}

	foreign, err := svc.store.GetSandbox(ctx, "project-1", "sandbox-b")
	if err != nil {
		t.Fatalf("get sandbox-b: %v", err)
	}
	if foreign.ResourcesObservedAt != nil {
		t.Errorf("pool-a wrote resources onto pool-b's sandbox: %v", foreign.ResourcesObservedAt)
	}
	owned, err := svc.store.GetSandbox(ctx, "project-1", "sandbox-a")
	if err != nil {
		t.Fatalf("get sandbox-a: %v", err)
	}
	if owned.ResourcesObservedAt == nil {
		t.Error("the owned sandbox was skipped along with the foreign one")
	}
}

// A sandbox that reported no counters has no observation time of its own, and
// must still be datable against the report that carried its disk usage.
func TestReportPoolResourcesDatesCounterlessSandboxByReportTime(t *testing.T) {
	svc, _ := newAgentServiceTestFixture(t)
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{
		Type:   auth.PrincipalTypePool,
		PoolID: "pool-a",
	})
	reportedAt := time.Now().UTC().Truncate(time.Second)

	err := svc.ReportPoolResources(ctx, "pool-a", services.ReportPoolResourcesBody{
		Report:    serverapi.PoolResourceReport{ReportedAt: reportedAt},
		Sandboxes: []serverapi.SandboxResourceConsumption{{SandboxId: "sandbox-a"}},
	})
	if err != nil {
		t.Fatalf("report resources: %v", err)
	}

	sandbox, err := svc.store.GetSandbox(ctx, "project-1", "sandbox-a")
	if err != nil {
		t.Fatalf("get sandbox: %v", err)
	}
	if sandbox.ResourcesObservedAt == nil || !sandbox.ResourcesObservedAt.Equal(reportedAt) {
		t.Fatalf("resourcesObservedAt = %v, want the report time %v", sandbox.ResourcesObservedAt, reportedAt)
	}
}

func TestReportPoolResourcesRequiresPoolPrincipal(t *testing.T) {
	svc, _ := newAgentServiceTestFixture(t)
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{
		Type:   auth.PrincipalTypeUser,
		UserID: "user-1",
	})

	err := svc.ReportPoolResources(ctx, "pool-a", services.ReportPoolResourcesBody{})
	var statusErr interface{ StatusCode() int }
	if err == nil || !errors.As(err, &statusErr) || statusErr.StatusCode() != 403 {
		t.Fatalf("report resources = %v, want 403 forbidden", err)
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
	// The first entry carries "state"/"lastEvent", the fields an older
	// sandbox-agent reported and this server no longer declares. Rows like it
	// are already stored, and relayed payloads come from whatever agent
	// version a sandbox runs, so an unknown field must not void the whole
	// report.
	got := reportedLastAccess(sessions(
		`[{"terminalId":"t1","primary":true,"state":"running","lastEvent":"Stop","attacherCount":0,"execStatus":"running","lastAccessedAt":"`+older.Format(time.RFC3339)+`"},`+
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
