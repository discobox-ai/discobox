package resources

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/discobox-ai/discobox/sandbox-agent/execs"
)

// TestNewSamplerReadsThisMachine runs the real ps and sysctl.
func TestNewSamplerReadsThisMachine(t *testing.T) {
	usage := NewSampler().Sample(context.Background())
	if usage.Source != "proc" || usage.ProcessCount < 2 {
		t.Fatalf("usage = %+v, want a rollup over this machine's processes", usage)
	}
	if usage.CPU.UsageUsec <= 0 || usage.Memory.ResidentBytes <= 0 {
		t.Fatalf("usage = %+v, want CPU time and resident memory", usage)
	}
	if len(usage.Processes) == 0 {
		t.Fatal("no candidate processes")
	}
}

func TestMachineMemory(t *testing.T) {
	current, limit, err := machineMemory()
	if err != nil {
		t.Fatal(err)
	}
	if current <= 0 || limit <= 0 || current > limit {
		t.Fatalf("memory = %d in use of %d, want some of the machine in use", current, limit)
	}
}

func TestKernelProcsKnowsThisProcess(t *testing.T) {
	procs, err := kernelProcs()
	if err != nil {
		t.Fatal(err)
	}
	self, ok := procs[os.Getpid()]
	if !ok || self.comm == "" || self.startTicks == 0 {
		t.Fatalf("this process = %+v (found %v), want a command and a start time", self, ok)
	}
}

func TestRunPSListsThisProcess(t *testing.T) {
	out, err := runPS(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range parsePS(string(out)) {
		if row.pid == os.Getpid() {
			if row.ppid != os.Getppid() || row.residentBytes <= 0 || row.args == "" {
				t.Fatalf("this process = %+v", row)
			}
			return
		}
	}
	t.Fatalf("ps did not list this process (pid %d)", os.Getpid())
}

func TestNewSamplerCollectsThisProcess(t *testing.T) {
	sample, err := NewSampler().Collect(context.Background(), execs.Exec{ID: "self", PID: int64(os.Getpid())})
	if err != nil {
		t.Fatal(err)
	}
	var data struct {
		Processes []map[string]any `json:"processes"`
	}
	if err := json.Unmarshal(sample.Data, &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Processes) == 0 || data.Processes[0]["pid"].(float64) != float64(os.Getpid()) {
		t.Fatalf("processes = %+v, want this process first", data.Processes)
	}
}
