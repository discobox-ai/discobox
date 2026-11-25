package sandboxruntime

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"

	"github.com/moby/moby/api/types/container"

	workerapimodel "github.com/discobox-ai/discobox/pool-agent/api/model"

	"github.com/discobox-ai/discobox/pkg/fsdurable"
)

// boxSize is a sandbox's CPU and memory as the control plane resolved them
// from the sandbox's own and its pool's (ADR 26-11-23-445 §2). The pool
// applies it as it is sent and never looks its pool's size up. A zero field
// is unset, which leaves the box to the runtime's default: no Docker limit on
// a container, the driver's own size for a machine.
type boxSize struct {
	CPUVCPUs    float64 `json:"cpuVcpus,omitempty"`
	MemoryBytes int64   `json:"memoryBytes,omitempty"`
}

// requestedSize is the size a create asks for; none for a nil request.
func requestedSize(req *workerapimodel.PoolSandboxCreateRequest) boxSize {
	if req == nil {
		return boxSize{}
	}
	sent, ok := req.Size.Get()
	if !ok {
		return boxSize{}
	}
	var size boxSize
	if cpu := sent.CpuVcpus.Or(0); cpu > 0 {
		size.CPUVCPUs = cpu
	}
	if memory := sent.MemoryBytes.Or(0); memory > 0 {
		size.MemoryBytes = memory
	}
	return size
}

// dockerResources limits a box's container to the size (ADR 26-11-23-445
// §4): its CPU as a quota, and its memory with swap equal to it, so the limit
// is not doubled by swap. An unset field sets no limit.
func (s boxSize) dockerResources() container.Resources {
	var resources container.Resources
	if s.CPUVCPUs > 0 {
		resources.NanoCPUs = int64(math.Round(s.CPUVCPUs * 1e9))
	}
	if s.MemoryBytes > 0 {
		resources.Memory = s.MemoryBytes
		resources.MemorySwap = s.MemoryBytes
	}
	return resources
}

// MachineSize is what a machine of a box's size is made at: whole vCPUs, rounded
// up as a pool VM's are (vmsize.FromPool), and its memory in bytes (ADR
// 26-11-23-445 §5). Zero is the driver's default for that field.
type MachineSize struct {
	CPUs        int    `json:"cpus,omitempty"`
	MemoryBytes uint64 `json:"memoryBytes,omitempty"`
}

func (s boxSize) machine() MachineSize {
	var size MachineSize
	if s.CPUVCPUs > 0 {
		size.CPUs = int(math.Ceil(s.CPUVCPUs))
	}
	if s.MemoryBytes > 0 {
		size.MemoryBytes = uint64(s.MemoryBytes)
	}
	return size
}

// planSize is the size as a snapshot's plan carries it: nil for none, so
// every key made before sizes is the key it was.
func (s MachineSize) planSize() *MachineSize {
	if s == (MachineSize{}) {
		return nil
	}
	return &s
}

// boxSizeRecordName is the pool's record of a sandbox's size, in its tree:
// what a machine made again for it with no create request in hand
// (remakeMachine) is made at.
const boxSizeRecordName = "size.json"

func (r *sandboxCore) boxSizeRecordPath(sandboxID string) string {
	return filepath.Join(r.sandboxRoot(sandboxID), boxSizeRecordName)
}

// recordBoxSize records the size a create sent, before anything is made at
// it.
func (r *sandboxCore) recordBoxSize(sandboxID string, size boxSize) error {
	data, err := json.Marshal(size)
	if err != nil {
		return err
	}
	path := r.boxSizeRecordPath(sandboxID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return fsdurable.WriteFile(path, data, 0o600)
}

// recordedBoxSize is the size the sandbox's last create sent; none for a
// sandbox made before sizes.
func (r *sandboxCore) recordedBoxSize(sandboxID string) (boxSize, error) {
	data, err := os.ReadFile(r.boxSizeRecordPath(sandboxID))
	if errors.Is(err, os.ErrNotExist) {
		return boxSize{}, nil
	}
	if err != nil {
		return boxSize{}, err
	}
	var size boxSize
	if err := json.Unmarshal(data, &size); err != nil {
		return boxSize{}, fmt.Errorf("decode %s: %w", boxSizeRecordName, err)
	}
	return size, nil
}
