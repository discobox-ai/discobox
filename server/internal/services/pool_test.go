package services

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/platform"
	"github.com/discobox-ai/discobox/pkg/pooloutbound"
	"github.com/discobox-ai/discobox/proxy/egress"
	"github.com/discobox-ai/discobox/server/internal/auth"
	"github.com/discobox-ai/discobox/server/internal/model"
)

func TestPoolAPIHealthMasksStaleFlagsWithoutChangingObservations(t *testing.T) {
	now := time.Now()
	before := now.Add(-time.Minute)
	pool := &model.Pool{ID: "pool-1", Ready: true, Schedulable: true,
		StatusReportedAt: &before, HealthCheckStartedAt: &now,
		ResourceLifecycle: model.ResourceLifecycle{State: model.PoolStateActive}}
	view, err := PoolToAPI(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	if string(view.Health.Or("")) != model.PoolHealthUnknown || view.Ready || view.Schedulable || string(view.State) != model.PoolStateActive {
		t.Fatalf("startup view = %+v", view)
	}
	if !pool.Ready || !pool.Schedulable {
		t.Fatal("projection mutated agent observations")
	}
	pool.StatusReportedAt = &now
	view, err = PoolToAPI(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	if string(view.Health.Or("")) != model.PoolHealthReady || !view.Ready || !view.Schedulable {
		t.Fatalf("fresh heartbeat view = %+v", view)
	}
	stale := now.Add(-model.PoolHeartbeatTimeout - time.Second)
	pool.StatusReportedAt = &stale
	pool.HealthCheckStartedAt = nil
	view, err = PoolToAPI(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	if string(view.Health.Or("")) != model.PoolHealthOffline || view.Ready || view.Schedulable {
		t.Fatalf("stale heartbeat view = %+v", view)
	}
}

// secretPool is a pool with every field a discobox must not be sent set to a
// value that names itself, so a leak of any of them shows in the JSON.
func secretPool() *model.Pool {
	now := time.Now()
	errorMessage := "leak-error"
	return &model.Pool{
		ID: "pool-1", ProjectID: "proj-1",
		PoolManifest: model.PoolManifest{Name: "archil", ProviderInstanceID: "prov-1", SandboxOS: "linux", CPUVCPUs: 4, MemoryBytes: 8 << 30},
		Proxy:        &pooloutbound.Intent{Mode: "explicit", HTTP: "http://leak-proxy.example:3128", CAPEM: "leak-ca"},
		Egress:       egress.Policy{DenyByDefault: true, Allow: []string{"leak-egress.example"}},
		Platform:     platform.Platform{OS: "linux", Arch: "arm64"},
		ImageKind:    platform.VM("libkrun"),
		PublicKey:    "leak-public-key", KeyType: "ed25519",
		Ready: true, Schedulable: true, AvailableCPUVCPUs: 2,
		Conditions:        model.RawJSON(`{"leak":"condition"}`),
		ProvisionProgress: model.RawJSON(`{"phase":"leak-phase"}`), ProvisionProgressAt: &now,
		Resources: model.RawJSON(`{"leak":"resources"}`), ResourcesReportedAt: &now,
		HostCredential: &model.PoolHostCredential{Key: "issued", Scope: "leak-scope", KeyID: "leak-key-id"},
		RegisteredAt:   &now, LastSeenAt: &now, StatusReportedAt: &now,
		ProviderInstance: &model.SandboxProviderInstance{ID: "prov-1", ProjectID: "proj-1", Type: "archil", Name: "leak-provider",
			Config: model.RawJSON(`{"region":"leak-region","endpoint":"https://leak-endpoint.example"}`), SecretFields: []string{"apiKey"}},
		ResourceLifecycle: model.ResourceLifecycle{State: model.PoolStateActive, DesiredState: model.DesiredStatePresent, ErrorMessage: &errorMessage},
	}
}

// A discobox reads a pool's identity, platform, room and lifecycle, and
// nothing else of it: not its provider instance or configuration, its agent's
// key, its host credential, proxy, egress, conditions, resources or errors
// (ADR 26-11-23-743 §2). A person reads all of it.
func TestPoolAPISendsADiscoboxOnlyWhatPlacementNeeds(t *testing.T) {
	sandboxCtx := auth.WithPrincipal(context.Background(), auth.Principal{Type: auth.PrincipalTypeSandbox, SandboxID: "sbx-1", ProjectID: "proj-1"})
	view, err := PoolToAPI(sandboxCtx, secretPool())
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(&view)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "leak") {
		t.Fatalf("a discobox's pool read carries a field it must not: %s", encoded)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	want := []string{"availableCpuVcpus", "availableMemoryBytes", "availableStorageBytes", "cpuVcpus", "createdAt", "degraded", "desiredState", "generation", "health", "id", "imageKind", "memoryBytes", "name", "observedGeneration", "platform", "projectId", "providerInstanceId", "ready", "sandboxOs", "schedulable", "state", "stateChangedAt", "storageBytes", "updatedAt"}
	got := make([]string, 0, len(fields))
	for key := range fields {
		got = append(got, key)
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("a discobox's pool read has fields %v, want %v", got, want)
	}
	if view.Platform.Or("") != "linux/arm64" || view.Name != "archil" || !view.Ready || view.AvailableCpuVcpus != 2 {
		t.Fatalf("a discobox's pool read lost what placement needs: %+v", view)
	}

	person, err := PoolToAPI(context.Background(), secretPool())
	if err != nil {
		t.Fatal(err)
	}
	if !person.ProviderInstance.Set || person.PublicKey.Or("") != "leak-public-key" || !person.HostCredential.Set {
		t.Fatalf("a person's pool read lost its provider, key or credential: %+v", person)
	}
}

// The pool a discobox reads embedded in a sandbox is narrowed as a pool read
// itself is.
func TestSandboxAPISendsADiscoboxItsPoolNarrowed(t *testing.T) {
	sandboxCtx := auth.WithPrincipal(context.Background(), auth.Principal{Type: auth.PrincipalTypeSandbox, SandboxID: "sbx-1", ProjectID: "proj-1"})
	out, err := SandboxToAPI(sandboxCtx, &model.Sandbox{ID: "sbx-2", ProjectID: "proj-1", CreatedByUserID: "user-1", PoolID: "pool-1", Pool: secretPool()}, SandboxUpgradeBasis{})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(&out.Pool.Value)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Pool.Set || strings.Contains(string(encoded), "leak") {
		t.Fatalf("a discobox's sandbox read carries its pool unnarrowed: %s", encoded)
	}
}
