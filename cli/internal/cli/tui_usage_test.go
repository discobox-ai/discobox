package cli

import (
	"encoding/json"
	"testing"

	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/tui"
)

// sandboxWithResources decodes a sandbox from the JSON the API actually
// returns, so the mapping is tested against the wire shape rather than against
// a hand-built struct that could drift from it.
func sandboxWithResources(t *testing.T, runtimeResources, poolResources string) apimodel.Sandbox {
	t.Helper()
	runtime := `"state":"ready","runtimeState":"running","displayState":"running","desiredState":"present","generation":1,"observedGeneration":1`
	if runtimeResources != "" {
		runtime += `,"resources":` + runtimeResources
	}
	pool := ""
	if poolResources != "" {
		pool = `,"pool":{"id":"pool-1","projectId":"project-1","name":"Default","providerInstanceId":"provider-1",` +
			`"cpuVcpus":0,"memoryBytes":0,"storageBytes":0,"ready":true,"schedulable":true,"degraded":false,` +
			`"availableCpuVcpus":0,"availableMemoryBytes":0,"availableStorageBytes":0,` +
			`"resources":` + poolResources + `,` +
			`"desiredState":"present","state":"active","generation":1,"observedGeneration":1,` +
			`"createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}`
	}
	raw := `{"id":"sandbox-1","projectId":"project-1","createdByUserId":"user-1","poolId":"pool-1",` +
		`"displayName":"box","config":{"name":"box","image":""},"runtime":{` + runtime + `}` + pool +
		`,"createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}`
	var sb apimodel.Sandbox
	if err := json.Unmarshal([]byte(raw), &sb); err != nil {
		t.Fatalf("decode sandbox: %v", err)
	}
	return sb
}

// A 24-core, 32GiB host with a 500GiB filesystem.
const poolCapacity = `{"reportedAt":"2026-08-27T12:00:00Z",` +
	`"cpu":{"usageUsec":1,"userUsec":1,"systemUsec":0,"vcpus":0.5,"capacityVcpus":24},` +
	`"memory":{"currentBytes":1,"capacityBytes":34359738368},` +
	`"storage":{"root":"/var/lib/discobox","filesystem":{"totalBytes":536870912000,"usedBytes":1,"freeBytes":1}}}`

func TestTUIUsageIsCPUShareAndMemoryAmount(t *testing.T) {
	// 6 of 24 vCPU is 25%. Memory is the resident 3.2GiB itself, which is 10%
	// of the host's 32GiB — the share colors the cell, the amount is drawn.
	// The charge and the virtual size are deliberately different numbers here,
	// so picking the wrong one shows up.
	consumption := `{"sandboxId":"sandbox-1","observedAt":"2026-08-27T12:00:00Z","source":"cgroup",` +
		`"cpu":{"usageUsec":1,"userUsec":1,"systemUsec":0,"vcpus":6.0},` +
		`"memory":{"currentBytes":9999999999,"virtualBytes":88888888888,"residentBytes":3435973836},` +
		`"processCount":4,` +
		`"storage":{"sandboxId":"sandbox-1","dataBytes":1,"configBytes":0,"sourcesBytes":0,` +
		`"secretsBytes":0,"originsBytes":0,"totalBytes":53687091200}}`

	got := toTUIUsage(sandboxWithResources(t, consumption, poolCapacity))

	if !got.Known || !got.DiskKnown {
		t.Fatalf("usage = %+v, want both measured", got)
	}
	if got.CPUPercent != 25 {
		t.Errorf("cpu = %d%%, want 25%% (6 of 24 vCPU)", got.CPUPercent)
	}
	// Resident, not the cgroup charge and not the virtual size.
	if got.MemoryBytes != 3435973836 {
		t.Errorf("memory = %d bytes, want the resident 3435973836", got.MemoryBytes)
	}
	if got.MemoryPercent != 10 {
		t.Errorf("memory share = %d%%, want 10%% of the host (colors the cell)", got.MemoryPercent)
	}
	if got.DiskBytes != 53687091200 || got.DiskPercent != 10 {
		t.Errorf("disk = %d bytes / %d%%, want 50GiB / 10%%", got.DiskBytes, got.DiskPercent)
	}
}

// The first report after an agent starts carries counters but no rate. That is
// not idle, and must not be drawn as 0%.
func TestTUIUsageIsUnknownWithoutARate(t *testing.T) {
	consumption := `{"sandboxId":"sandbox-1","observedAt":"2026-08-27T12:00:00Z","source":"cgroup",` +
		`"cpu":{"usageUsec":8204113000,"userUsec":1,"systemUsec":0},` +
		`"memory":{"currentBytes":1,"virtualBytes":1,"residentBytes":3435973836},"processCount":4}`

	got := toTUIUsage(sandboxWithResources(t, consumption, poolCapacity))

	if got.Known {
		t.Errorf("a sandbox with counters but no rate was drawn as measured: %+v", got)
	}
	if got.CPUPercent != 0 || got.MemoryBytes != 0 {
		t.Errorf("unmeasured usage carried figures: %+v", got)
	}
}

// Disk is walked on a slower schedule, so a sandbox created since the last
// sweep is measured for cpu and not yet for disk.
func TestTUIUsageKnowsCPUBeforeDisk(t *testing.T) {
	consumption := `{"sandboxId":"sandbox-1","observedAt":"2026-08-27T12:00:00Z","source":"cgroup",` +
		`"cpu":{"usageUsec":1,"userUsec":1,"systemUsec":0,"vcpus":6.0},` +
		`"memory":{"currentBytes":1,"virtualBytes":1,"residentBytes":3435973836},"processCount":4}`

	got := toTUIUsage(sandboxWithResources(t, consumption, poolCapacity))

	if !got.Known {
		t.Fatal("cpu should be measured")
	}
	if got.DiskKnown || got.DiskBytes != 0 {
		t.Errorf("disk was claimed before it was walked: %+v", got)
	}
}

// Without a pool there is no denominator, so there is no share to draw.
func TestTUIUsageNeedsADenominator(t *testing.T) {
	consumption := `{"sandboxId":"sandbox-1","observedAt":"2026-08-27T12:00:00Z","source":"cgroup",` +
		`"cpu":{"usageUsec":1,"userUsec":1,"systemUsec":0,"vcpus":6.0},` +
		`"memory":{"currentBytes":1,"virtualBytes":1,"residentBytes":1},"processCount":4}`

	got := toTUIUsage(sandboxWithResources(t, consumption, ""))

	if got.Known {
		t.Errorf("a share was drawn with nothing to divide by: %+v", got)
	}
}

func TestTUIUsageIsEmptyWithoutAReport(t *testing.T) {
	if got := toTUIUsage(sandboxWithResources(t, "", poolCapacity)); got != (tui.Usage{}) {
		t.Errorf("usage = %+v, want zero for a sandbox that never reported", got)
	}
}

// A rate sampled a moment apart from its denominator can exceed it; "103%" is
// an artifact rather than a finding.
func TestPercentOfClamps(t *testing.T) {
	if got := percentOf(30, 24); got != 100 {
		t.Errorf("percentOf(30, 24) = %d, want 100", got)
	}
	if got := percentOf(1, 0); got != 0 {
		t.Errorf("percentOf(1, 0) = %d, want 0", got)
	}
	if got := percentOf(1, 3); got != 33 {
		t.Errorf("percentOf(1, 3) = %d, want 33", got)
	}
}
