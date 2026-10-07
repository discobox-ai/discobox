package resources

import (
	"context"
	"encoding/json"
	"os"
	"runtime"
	"time"

	"github.com/discobox-ai/discobox/sandbox-agent/execs"
	"github.com/discobox-ai/discobox/sandbox-agent/store"
	"github.com/discobox-ai/discobox/sandbox-agent/terminal"
)

// Sampler is the observation seam for resource counters (ADR 0145 §4). Every
// platform the sandbox agent runs on has one, made by NewSampler, and the
// contract is the same on each: cumulative counters, never a rate (ADR 0071).
// A sampler keeps no sampling state — sandbox-agent's status is computed fresh
// on every call and never pushed on its own initiative (ADR 0030) — except
// where a platform has no cumulative counter of its own to read: darwin's
// remembers the CPU time of exited processes, which a cgroup keeps on Linux.
type Sampler interface {
	// Sample reads the whole sandbox's usage: its totals and its candidate
	// processes.
	Sample(ctx context.Context) Usage
	// Collect reads one exec's opaque resource snapshot, the platform's own
	// view of the exec and the processes under it.
	Collect(ctx context.Context, ex execs.Exec) (store.ResourceSample, error)
}

// execSnapshot is the part of an exec's resource snapshot every platform
// writes: the exec, and the host it runs on.
func execSnapshot(ex execs.Exec) map[string]any {
	host := map[string]any{
		"goos":   runtime.GOOS,
		"goarch": runtime.GOARCH,
	}
	if hostname, err := os.Hostname(); err == nil {
		host["hostname"] = hostname
	}
	return map[string]any{
		"terminal": map[string]any{
			"id":        ex.ID,
			"harnessId": terminal.HarnessID(ex),
			"status":    ex.Status,
			"unit":      ex.Unit,
			"pid":       ex.PID,
			"workdir":   ex.Workdir,
			"metadata":  ex.Metadata,
		},
		"host": host,
	}
}

// resourceSample stores data as an exec's snapshot, under the name of the
// source that read it.
func resourceSample(ex execs.Exec, sampledAt time.Time, source string, data map[string]any) (store.ResourceSample, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return store.ResourceSample{}, err
	}
	return store.ResourceSample{
		TerminalID: ex.ID,
		SampledAt:  sampledAt,
		Source:     source,
		Data:       raw,
	}, nil
}
