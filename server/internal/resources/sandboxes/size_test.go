package sandboxes

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/internal/sandboxexport"
	"github.com/discobox-ai/discobox/server/internal/services"
)

const gib = 1 << 30

// sizedProvider is a provider that declares the size fields it acts on.
type sizedProvider struct {
	*recordingProvider
	definition sandbox.ProviderDefinition
}

func (p *sizedProvider) Definition() sandbox.ProviderDefinition { return p.definition }

// sizedPool registers a provider type of definition, and stores a provider
// instance of it and a pool on it of the size given.
func sizedPool(ctx context.Context, t *testing.T, svc *Service, providerType string, definition sandbox.ProviderDefinition, cpuVCPUs float64, memoryBytes int64) string {
	t.Helper()
	svc.sandboxProviders.RegisterProvider(providerType, &sizedProvider{recordingProvider: &recordingProvider{}, definition: definition})
	instance := &model.SandboxProviderInstance{ID: "provider-" + providerType, ProjectID: "project-1", Type: providerType, Name: "My " + providerType}
	if err := svc.store.CreateSandboxProviderInstance(ctx, instance); err != nil {
		t.Fatal(err)
	}
	poolID := "pool-" + providerType
	if err := svc.store.CreatePool(ctx, &model.Pool{ID: poolID, ProjectID: "project-1", PoolManifest: model.PoolManifest{
		Name: poolID, ProviderInstanceID: instance.ID, CPUVCPUs: cpuVCPUs, MemoryBytes: memoryBytes,
	}}); err != nil {
		t.Fatal(err)
	}
	return poolID
}

func createSized(ctx context.Context, svc *Service, name, poolID string, cpuVCPUs float64, memoryBytes int64) (*model.Sandbox, error) {
	config := serverapi.SandboxCreateConfig{Name: name}
	if cpuVCPUs != 0 {
		config.CpuVcpus = serverapi.NewOptFloat64(cpuVCPUs)
	}
	if memoryBytes != 0 {
		config.MemoryBytes = serverapi.NewOptInt64(memoryBytes)
	}
	return svc.CreateSandbox(ctx, "project-1", services.CreateSandboxBody{
		HarnessName: serverapi.NewOptString("codex"),
		PoolId:      serverapi.NewOptString(poolID),
		Config:      config,
	})
}

func requireSizeRefused(t *testing.T, err error, mentions ...string) {
	t.Helper()
	var status apperrors.StatusError
	if !errors.As(err, &status) || status.Status != http.StatusBadRequest {
		t.Fatalf("error = %v, want a 400", err)
	}
	for _, want := range mentions {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to mention %q", err, want)
		}
	}
}

// A box's size is resolved at its create field by field: its own, else its
// pool's. It is stored, returned on its read, and sent to its pool as it is
// (ADR 26-11-23-445 §§1-3).
func TestABoxSizeOverridesItsPoolsFieldByField(t *testing.T) {
	ctx, svc, st, _ := transferFixture(t)
	configuredHarness(t, st, "codex", "Codex")
	poolID := sizedPool(ctx, t, svc, "sized", sandbox.ProviderDefinition{SizeFields: []sandbox.SizeField{sandbox.SizeCPU, sandbox.SizeMemory}}, 2, 4*gib)

	for _, tc := range []struct {
		name       string
		cpu        float64
		memory     int64
		wantCPU    float64
		wantMemory int64
	}{
		{"own-cpu", 6, 0, 6, 4 * gib},
		{"own-memory", 0, 16 * gib, 2, 16 * gib},
		{"pools", 0, 0, 2, 4 * gib},
		{"own", 0.5, gib, 0.5, gib},
	} {
		created, err := createSized(ctx, svc, tc.name, poolID, tc.cpu, tc.memory)
		if err != nil {
			t.Fatalf("%s: create: %v", tc.name, err)
		}
		sb, err := st.GetSandbox(ctx, "project-1", created.ID)
		if err != nil {
			t.Fatal(err)
		}
		if sb.CPUVCPUs != tc.wantCPU || sb.MemoryBytes != tc.wantMemory {
			t.Errorf("%s: stored size = %g vCPUs, %d bytes; want %g, %d", tc.name, sb.CPUVCPUs, sb.MemoryBytes, tc.wantCPU, tc.wantMemory)
		}
		read, err := services.SandboxToAPI(ctx, sb, services.SandboxUpgradeBasis{})
		if err != nil {
			t.Fatal(err)
		}
		if read.Config.CpuVcpus.Or(0) != tc.wantCPU || read.Config.MemoryBytes.Or(0) != tc.wantMemory {
			t.Errorf("%s: read size = %v, %v; want %g, %d", tc.name, read.Config.CpuVcpus, read.Config.MemoryBytes, tc.wantCPU, tc.wantMemory)
		}
		opts, err := (&SandboxReconciler{store: st}).createOptionsFromSandbox(ctx, sb)
		if err != nil {
			t.Fatal(err)
		}
		if opts.Size != (sandbox.Size{CPUVCPUs: tc.wantCPU, MemoryBytes: tc.wantMemory}) {
			t.Errorf("%s: size sent to the pool = %+v", tc.name, opts.Size)
		}
	}

	// A later change to the pool's size reaches new boxes only.
	pool, err := st.GetPool(ctx, "project-1", poolID)
	if err != nil {
		t.Fatal(err)
	}
	pool.CPUVCPUs = 8
	if err := st.UpdatePool(ctx, pool); err != nil {
		t.Fatal(err)
	}
	sandboxes, err := st.ListSandboxes(ctx, "project-1", "", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, sb := range sandboxes {
		if sb.Name == "pools" && sb.CPUVCPUs != 2 {
			t.Errorf("a box made at its pool's size moved to %g vCPUs with its pool", sb.CPUVCPUs)
		}
	}
}

// A provider that does not act on a box's size refuses one that asks for it,
// naming the field, the provider instance and its type, rather than make the
// box at another size; an unsized box on it is made, and a size its pool
// holds from before is not passed on (ADR 26-11-23-445 §6). A negative size
// is refused everywhere.
func TestABoxSizeAProviderDoesNotActOnIsRefused(t *testing.T) {
	ctx, svc, st, _ := transferFixture(t)
	configuredHarness(t, st, "codex", "Codex")
	poolID := sizedPool(ctx, t, svc, "unsized", sandbox.ProviderDefinition{}, 4, 8*gib)

	_, err := createSized(ctx, svc, "asks", poolID, 0, 2*gib)
	requireSizeRefused(t, err, "memoryBytes", `"My unsized"`, "unsized provider", "takes no size settings")
	if taken, _ := st.SandboxNameTaken(ctx, "project-1", "asks"); taken {
		t.Error("a refused box was stored")
	}

	created, err := createSized(ctx, svc, "plain", poolID, 0, 0)
	if err != nil {
		t.Fatalf("unsized box: %v", err)
	}
	sb, err := st.GetSandbox(ctx, "project-1", created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sb.CPUVCPUs != 0 || sb.MemoryBytes != 0 {
		t.Errorf("unsized box on a provider that sizes nothing = %g vCPUs, %d bytes; want none", sb.CPUVCPUs, sb.MemoryBytes)
	}

	sizedPoolID := sizedPool(ctx, t, svc, "sized", sandbox.ProviderDefinition{SizeFields: []sandbox.SizeField{sandbox.SizeCPU, sandbox.SizeMemory}}, 0, 0)
	_, err = createSized(ctx, svc, "negative", sizedPoolID, -1, 0)
	requireSizeRefused(t, err, "cpuVcpus", "negative")
}

// On a provider that runs every box of a pool in one VM, a box may not be
// larger than a size its pool sets, and where the pool sets none it is not
// checked (ADR 26-11-23-445 §7); a box that asks for none is not given the
// pool's size, which is the VM's (§3).
func TestABoxLargerThanItsPoolsVMIsRefused(t *testing.T) {
	ctx, svc, st, _ := transferFixture(t)
	configuredHarness(t, st, "codex", "Codex")
	definition := sandbox.ProviderDefinition{SizeFields: []sandbox.SizeField{sandbox.SizeCPU, sandbox.SizeMemory}, BoxesInPoolVM: true}
	poolID := sizedPool(ctx, t, svc, "vm", definition, 4, 8*gib)

	_, err := createSized(ctx, svc, "too-many-cpus", poolID, 6, 0)
	requireSizeRefused(t, err, "cpuVcpus", `pool "pool-vm"`)
	_, err = createSized(ctx, svc, "too-much-memory", poolID, 0, 16*gib)
	requireSizeRefused(t, err, "memoryBytes", `pool "pool-vm"`)
	if _, err := createSized(ctx, svc, "fits", poolID, 4, 2*gib); err != nil {
		t.Fatalf("a box that fits its pool's VM: %v", err)
	}
	// An unset box has the whole VM: the pool's size is the VM's, never
	// copied to a box, where the driver may have clamped the VM below it.
	created, err := createSized(ctx, svc, "whole-vm", poolID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	sb, err := st.GetSandbox(ctx, "project-1", created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sb.CPUVCPUs != 0 || sb.MemoryBytes != 0 {
		t.Errorf("unset box on a pool VM = %g vCPUs, %d bytes; want none, the whole VM", sb.CPUVCPUs, sb.MemoryBytes)
	}

	hostSized := sizedPool(ctx, t, svc, "vm-host", definition, 0, 0)
	if _, err := createSized(ctx, svc, "host-sized", hostSized, 64, 512*gib); err != nil {
		t.Fatalf("a box on a pool whose VM is the host's default: %v", err)
	}
}

// sizedTreeProvider is the transfer fixture's provider, declaring sizes.
type sizedTreeProvider struct {
	*treeProvider
	definition sandbox.ProviderDefinition
}

func (p *sizedTreeProvider) Definition() sandbox.ProviderDefinition { return p.definition }

// An import takes its new pool's default size, as a create asking for none
// does: the archive's size was resolved against the pool it left, and a move
// is never refused for it.
func TestAnImportTakesItsNewPoolsSize(t *testing.T) {
	ctx, svc, st, provider := transferFixture(t)
	configuredHarness(t, st, "codex", "Codex")
	svc.sandboxProviders.RegisterProvider("test", &sizedTreeProvider{treeProvider: provider, definition: sandbox.ProviderDefinition{
		SizeFields: []sandbox.SizeField{sandbox.SizeCPU, sandbox.SizeMemory},
	}})
	pool, err := st.GetPool(ctx, "project-1", "pool-1")
	if err != nil {
		t.Fatal(err)
	}
	pool.CPUVCPUs = 2
	if err := st.UpdatePool(ctx, pool); err != nil {
		t.Fatal(err)
	}
	archive := exportArchive(t, func(m *sandboxexport.Manifest) {
		m.Sandbox.Manifest.CPUVCPUs, m.Sandbox.Manifest.MemoryBytes = 8, 16*gib
	}, map[string]string{"data/.bashrc": "x\n"})
	result, err := svc.ImportSandbox(ctx, "project-1", bytes.NewReader(archive), services.SandboxImportOptions{})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	sb, err := st.GetSandbox(ctx, "project-1", result.Sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sb.CPUVCPUs != 2 || sb.MemoryBytes != 0 {
		t.Fatalf("imported size = %g vCPUs, %d bytes; want its new pool's, 2 and none", sb.CPUVCPUs, sb.MemoryBytes)
	}
}
