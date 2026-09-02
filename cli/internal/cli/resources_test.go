package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// poolWithResources is a pool that has reported: 8.10 vCPU of 16 in use, of
// which the two discoboxes below account for 3.75 — so 4.35 is overhead, which
// is the number the view exists to surface.
const poolWithResources = `{"id":"pool-1","projectId":"project-1","name":"Default","providerInstanceId":"provider-1",` +
	`"cpuVcpus":0,"memoryBytes":0,"storageBytes":0,"ready":true,"schedulable":true,"degraded":false,` +
	`"availableCpuVcpus":0,"availableMemoryBytes":0,"availableStorageBytes":0,` +
	`"resources":{"reportedAt":"2026-08-27T12:00:00Z",` +
	`"cpu":{"usageUsec":900000000,"userUsec":700000000,"systemUsec":200000000,"vcpus":8.10,"capacityVcpus":16},` +
	`"memory":{"currentBytes":25769803776,"availableBytes":8589934592},` +
	`"storage":{"root":"/var/lib/discobox","filesystem":{"totalBytes":536870912000,"usedBytes":128849018880,"freeBytes":408021893120},` +
	`"walk":{"observedAt":"2026-08-27T11:52:00Z","durationMillis":11400,"intervalSeconds":570,"nextScanAt":"2026-08-27T12:01:30Z",` +
	`"cacheBytes":44023414784,"buildBytes":9663676416}}},` +
	`"resourcesReportedAt":"2026-08-27T12:00:00Z",` +
	`"desiredState":"present","state":"active","generation":1,"observedGeneration":1,` +
	`"createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}`

// sandboxRuntime builds a discobox row with the given resource consumption
// spliced into its runtime object.
func sandboxRuntime(id, name, resources string) string {
	runtime := `"state":"ready","runtimeState":"running","displayState":"running","desiredState":"present","generation":1,"observedGeneration":1`
	if resources != "" {
		runtime += `,"resources":` + resources
	}
	return `{"id":"` + id + `","projectId":"project-1","createdByUserId":"user-1","poolId":"pool-1",` +
		`"displayName":"` + name + `","config":{"name":"` + name + `","image":""},` +
		`"runtime":{` + runtime + `},` +
		`"createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}`
}

const busySandboxResources = `{"sandboxId":"sandbox-1","observedAt":"2026-08-27T12:00:00Z","source":"cgroup",` +
	`"cpu":{"usageUsec":8204113000,"userUsec":6000000000,"systemUsec":2204113000,"vcpus":3.71,"windowSeconds":30},` +
	`"memory":{"currentBytes":6442450944,"virtualBytes":12884901888,"residentBytes":5368709120,"fileBytes":1073741824},` +
	`"processCount":42,` +
	`"processes":[` +
	`{"pid":100,"command":"node","cmdline":"node /work/node_modules/.bin/vitest","cpuUsec":3400000,"vcpus":3.40,"virtualBytes":8589934592,"residentBytes":4294967296},` +
	`{"pid":221,"command":"rust-analyzer","cpuUsec":220000,"vcpus":0.22,"virtualBytes":3221225472,"residentBytes":1932735283}],` +
	`"storage":{"sandboxId":"sandbox-1","dataBytes":2147483648,"configBytes":4096,"sourcesBytes":838860800,"secretsBytes":0,"originsBytes":100663296,"totalBytes":3087011840}}`

const idleSandboxResources = `{"sandboxId":"sandbox-2","observedAt":"2026-08-27T12:00:00Z","source":"cgroup",` +
	`"cpu":{"usageUsec":5000000,"userUsec":4000000,"systemUsec":1000000,"vcpus":0.04,"windowSeconds":30},` +
	`"memory":{"currentBytes":322122547,"virtualBytes":1073741824,"residentBytes":268435456},` +
	`"processCount":12,` +
	`"storage":{"sandboxId":"sandbox-2","dataBytes":1073741824,"configBytes":4096,"sourcesBytes":104857600,"secretsBytes":0,"originsBytes":0,"totalBytes":1178603520}}`

func runCLI(t *testing.T, serverURL string, args ...string) string {
	t.Helper()
	root := NewRootCommand()
	setFlag(t, root, "server", serverURL)
	setFlag(t, root, "project", "project-1")
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		t.Fatalf("execute %v: %v\noutput:\n%s", args, err, out.String())
	}
	return out.String()
}

func TestPoolResourcesRanksSandboxesAndShowsOverhead(t *testing.T) {
	server := completionServer(t, map[string]string{
		"/projects/project-1/pools":        `{"pools":[` + poolWithResources + `]}`,
		"/projects/project-1/pools/pool-1": poolWithResources,
		"/projects/project-1/sandboxes": `{"sandboxes":[` +
			sandboxRuntime("sandbox-2", "docs", idleSandboxResources) + `,` +
			sandboxRuntime("sandbox-1", "fix-login", busySandboxResources) + `]}`,
	})

	out := runCLI(t, server.URL, "admin", "pool", "resources", "pool-1")

	// The busy discobox must lead, whatever order the API returned them in:
	// ranking is the reason this view exists.
	busy := strings.Index(out, "sandbox-1")
	idle := strings.Index(out, "sandbox-2")
	if busy < 0 || idle < 0 {
		t.Fatalf("both discoboxes should appear:\n%s", out)
	}
	if busy > idle {
		t.Errorf("the busiest discobox did not rank first:\n%s", out)
	}

	for _, want := range []string{
		"8.10 of 16.00 vCPU", // pool load against capacity
		"(51%)",              // share of the pool
		"3.71",               // the busy discobox's rate
		"discoboxes (2)",     // what the discoboxes account for
		"3.75",               // their sum
		"pool services",      // measured separately, not derived by subtraction
		"8.10",               // the services' own figure
		"total",              // and the two added
		"11.85",              // 3.75 + 8.10
		"cache 41.0GiB (shared)",
		"11.4s",       // what the sweep cost
		"every 9m30s", // and the interval that cost bought
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// A discobox whose agent has not reported must read as unmeasured, not idle: a
// zero in the CPU column is a claim that it is doing nothing.
func TestPoolResourcesShowsUnreportedSandboxAsUnmeasured(t *testing.T) {
	server := completionServer(t, map[string]string{
		"/projects/project-1/pools":        `{"pools":[` + poolWithResources + `]}`,
		"/projects/project-1/pools/pool-1": poolWithResources,
		"/projects/project-1/sandboxes":    `{"sandboxes":[` + sandboxRuntime("sandbox-3", "cold", "") + `]}`,
	})

	out := runCLI(t, server.URL, "admin", "pool", "resources", "pool-1")

	if strings.Contains(out, "0.00") {
		t.Errorf("an unreported discobox was drawn as idle rather than unmeasured:\n%s", out)
	}
	if !strings.Contains(out, "sandbox-3") {
		t.Errorf("an unreported discobox was dropped from the listing:\n%s", out)
	}
}

// A pool that has never reported must say so rather than draw a table of
// zeroes, which would read as a measurement.
func TestPoolResourcesExplainsAPoolThatHasNotReported(t *testing.T) {
	const bare = `{"id":"pool-1","projectId":"project-1","name":"Default","providerInstanceId":"provider-1",` +
		`"cpuVcpus":0,"memoryBytes":0,"storageBytes":0,"ready":false,"schedulable":false,"degraded":false,` +
		`"availableCpuVcpus":0,"availableMemoryBytes":0,"availableStorageBytes":0,` +
		`"desiredState":"present","state":"pending","generation":1,"observedGeneration":1,` +
		`"createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}`
	server := completionServer(t, map[string]string{
		"/projects/project-1/pools":        `{"pools":[` + bare + `]}`,
		"/projects/project-1/pools/pool-1": bare,
		"/projects/project-1/sandboxes":    `{"sandboxes":[]}`,
	})

	out := runCLI(t, server.URL, "admin", "pool", "resources", "pool-1")

	if !strings.Contains(out, "has not reported resource usage yet") {
		t.Errorf("a pool with no report did not say so:\n%s", out)
	}
}

func TestSandboxResourcesRanksProcessesAndSplitsMemory(t *testing.T) {
	server := completionServer(t, map[string]string{
		"/projects/project-1/sandboxes":           `{"sandboxes":[` + sandboxRuntime("sandbox-1", "fix-login", busySandboxResources) + `]}`,
		"/projects/project-1/sandboxes/sandbox-1": sandboxRuntime("sandbox-1", "fix-login", busySandboxResources),
	})

	out := runCLI(t, server.URL, "admin", "discobox", "resources", "sandbox-1")

	for _, want := range []string{
		"3.71 vCPU",
		"over 30s",
		"(uncapped)", // nothing caps a discobox today; say so
		"[cgroup]",   // the totals are authoritative
		"6.0GiB charged, 5.0GiB resident, 12.0GiB virtual",
		"reclaimable page cache",
		"node /work/node_modules/.bin/vitest", // the full argv names the culprit
		"rust-analyzer",
		"data 2.0GiB",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}

	// Ranked by rate: vitest above rust-analyzer.
	if strings.Index(out, "vitest") > strings.Index(out, "rust-analyzer") {
		t.Errorf("processes were not ranked by rate:\n%s", out)
	}
}

func TestSandboxResourcesExplainsADiscoboxThatHasNotReported(t *testing.T) {
	server := completionServer(t, map[string]string{
		"/projects/project-1/sandboxes":           `{"sandboxes":[` + sandboxRuntime("sandbox-3", "cold", "") + `]}`,
		"/projects/project-1/sandboxes/sandbox-3": sandboxRuntime("sandbox-3", "cold", ""),
	})

	out := runCLI(t, server.URL, "admin", "discobox", "resources", "sandbox-3")

	if !strings.Contains(out, "no resource report yet") {
		t.Errorf("a discobox with no report did not say so:\n%s", out)
	}
}

// The JSON form is for scripting, so it must carry the contract shape rather
// than the table's flattened rows.
func TestPoolResourcesJSONCarriesTheReportedShape(t *testing.T) {
	server := completionServer(t, map[string]string{
		"/projects/project-1/pools":        `{"pools":[` + poolWithResources + `]}`,
		"/projects/project-1/pools/pool-1": poolWithResources,
		"/projects/project-1/sandboxes":    `{"sandboxes":[` + sandboxRuntime("sandbox-1", "fix-login", busySandboxResources) + `]}`,
	})

	out := runCLI(t, server.URL, "--output", "json", "admin", "pool", "resources", "pool-1")

	for _, want := range []string{`"usageUsec"`, `"vcpus"`, `"capacityVcpus"`, `"cacheBytes"`, `"sandboxId"`} {
		if !strings.Contains(out, want) {
			t.Errorf("json output missing %q:\n%s", want, out)
		}
	}
}

func TestFormatAgeReadsAsElapsed(t *testing.T) {
	if got := formatAge(time.Time{}); got != "never" {
		t.Errorf("zero time = %q, want never", got)
	}
	if got := formatAge(time.Now().Add(-90 * time.Second)); got != "1m ago" {
		t.Errorf("90s ago = %q, want 1m ago", got)
	}
}

// A resource blob this build cannot read must cost the resource line, never the
// command. The blob is written by whatever agent version a pool is running, and
// a strictly-typed field made one stale row 500 every pool listing.
func TestPoolResourcesToleratesABlobFromAnotherVersion(t *testing.T) {
	const stale = `{"id":"pool-1","projectId":"project-1","name":"Default","providerInstanceId":"provider-1",` +
		`"cpuVcpus":0,"memoryBytes":0,"storageBytes":0,"ready":true,"schedulable":true,"degraded":false,` +
		`"availableCpuVcpus":0,"availableMemoryBytes":0,"availableStorageBytes":0,` +
		// The pre-walk shape: cacheBytes flat on storage, where nothing expects it now.
		`"resources":{"reportedAt":"2026-08-27T12:00:00Z","cpu":{"usageUsec":1,"userUsec":1,"systemUsec":0},` +
		`"memory":{"currentBytes":1},"storage":{"root":"/var/lib/discobox",` +
		`"filesystem":{"totalBytes":1,"usedBytes":1,"freeBytes":0},"cacheBytes":99,"scanMillis":5}},` +
		`"desiredState":"present","state":"active","generation":1,"observedGeneration":1,` +
		`"createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}`
	server := completionServer(t, map[string]string{
		"/projects/project-1/pools":        `{"pools":[` + stale + `]}`,
		"/projects/project-1/pools/pool-1": stale,
		"/projects/project-1/sandboxes":    `{"sandboxes":[]}`,
	})

	// The command must succeed at all — runCLI fails the test if it does not.
	out := runCLI(t, server.URL, "admin", "pool", "resources", "pool-1")
	if out == "" {
		t.Error("no output for a pool carrying an older resource blob")
	}
	// An extra field an older agent wrote is tolerated, so the report still reads.
	if strings.Contains(out, "has not reported resource usage yet") {
		t.Errorf("an older blob's extra field discarded the whole report:\n%s", out)
	}
}

// A sweep written by an agent that predates the data total must still read.
// Making that figure required would have cost the whole report, which draws as
// a pool nothing has ever measured — over one field nothing depends on.
func TestPoolResourcesReadsASweepWithoutTheDataTotal(t *testing.T) {
	const older = `{"id":"pool-1","projectId":"project-1","name":"Default","providerInstanceId":"provider-1",` +
		`"cpuVcpus":0,"memoryBytes":0,"storageBytes":0,"ready":true,"schedulable":true,"degraded":false,` +
		`"availableCpuVcpus":0,"availableMemoryBytes":0,"availableStorageBytes":0,` +
		`"resources":{"reportedAt":"2026-08-27T12:00:00Z",` +
		`"cpu":{"usageUsec":1,"userUsec":1,"systemUsec":0,"vcpus":8.10,"capacityVcpus":16},` +
		`"memory":{"currentBytes":25769803776},` +
		`"storage":{"root":"/var/lib/discobox","filesystem":{"totalBytes":536870912000,"usedBytes":1,"freeBytes":408021893120},` +
		// The sweep as it was before dataBytes existed.
		`"walk":{"observedAt":"2026-08-27T11:52:00Z","durationMillis":11400,"intervalSeconds":570,` +
		`"nextScanAt":"2026-08-27T12:01:30Z","cacheBytes":44023414784,"buildBytes":9663676416}}},` +
		`"desiredState":"present","state":"active","generation":1,"observedGeneration":1,` +
		`"createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}`
	server := completionServer(t, map[string]string{
		"/projects/project-1/pools":        `{"pools":[` + older + `]}`,
		"/projects/project-1/pools/pool-1": older,
		"/projects/project-1/sandboxes":    `{"sandboxes":[]}`,
	})

	out := runCLI(t, server.URL, "admin", "pool", "resources", "pool-1")

	if strings.Contains(out, "has not reported resource usage yet") {
		t.Errorf("a sweep without the data total discarded the whole report:\n%s", out)
	}
	// Everything the older agent did send still reads.
	if !strings.Contains(out, "cache 41.0GiB (shared)") {
		t.Errorf("output lost the figures the older agent did send:\n%s", out)
	}
}
