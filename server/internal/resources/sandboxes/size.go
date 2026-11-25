package sandboxes

import (
	"fmt"
	"net/http"

	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/model"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
)

// resolveSandboxSize is the size a box is made at on pool, from the size it
// asks for, field by field (ADR 26-11-23-445): its own value if set, else its
// pool's default, else zero, which is none (§2). On a provider whose boxes
// share one VM sized from the pool's size, that size is the VM's and is no
// box's default: an unset box has the whole VM (§3), which a stored copy of
// the pool's size would not be once the driver clamps the VM below it (vz
// caps it at the host's cores) or the pool shrinks, and Docker refuses a
// container a limit past the CPUs its daemon has. It refuses a non-zero field the
// pool's provider does not act on, naming the field, the provider instance
// and its type, rather than accept it and make the box at another size (§6);
// and on a provider that runs every box of a pool in one VM, a field larger
// than the VM the pool sets, since no box is larger than the VM it runs in
// (§7). Nothing is checked against what the pool reports as available (§7,
// ADR 0029 §2).
func (s *Service) resolveSandboxSize(provider *model.SandboxProviderInstance, pool *model.Pool, cpuVCPUs float64, memoryBytes int64) (float64, int64, error) {
	definition := s.providerDefinition(provider.Type)
	asked := []sandbox.SizeValue{
		{Field: sandbox.SizeCPU, Value: cpuVCPUs},
		{Field: sandbox.SizeMemory, Value: float64(memoryBytes)},
	}
	if err := definition.CheckSize("discobox", provider.Name, provider.Type, asked); err != nil {
		return 0, 0, apperrors.NewStatusError(http.StatusBadRequest, err.Error())
	}
	if definition.BoxesInPoolVM {
		if pool.CPUVCPUs > 0 && cpuVCPUs > pool.CPUVCPUs {
			return 0, 0, apperrors.NewStatusError(http.StatusBadRequest, fmt.Sprintf(
				"discobox cpuVcpus %g is more than pool %q's VM has (%g): every discobox of the pool runs in that VM; ask for no more, or leave it unset for the whole VM",
				cpuVCPUs, pool.Name, pool.CPUVCPUs))
		}
		if pool.MemoryBytes > 0 && memoryBytes > pool.MemoryBytes {
			return 0, 0, apperrors.NewStatusError(http.StatusBadRequest, fmt.Sprintf(
				"discobox memoryBytes %d is more than pool %q's VM has (%d): every discobox of the pool runs in that VM; ask for no more, or leave it unset for the whole VM",
				memoryBytes, pool.Name, pool.MemoryBytes))
		}
	}
	// A pool's size is its boxes' default only where its provider acts on
	// it, and sizes no VM from it: one set before the provider stopped acting
	// on it is not sent to a box that would ignore it.
	if definition.BoxesInPoolVM {
		return cpuVCPUs, memoryBytes, nil
	}
	if cpuVCPUs == 0 && definition.ActsOnSize(sandbox.SizeCPU) {
		cpuVCPUs = pool.CPUVCPUs
	}
	if memoryBytes == 0 && definition.ActsOnSize(sandbox.SizeMemory) {
		memoryBytes = pool.MemoryBytes
	}
	return cpuVCPUs, memoryBytes, nil
}

// providerDefinition is what the provider type declares. A type this server
// has no definition for declares nothing, which is correct: a provider that
// is not registered here acts on nothing here.
func (s *Service) providerDefinition(providerType string) sandbox.ProviderDefinition {
	if s.sandboxProviders == nil {
		return sandbox.ProviderDefinition{}
	}
	definition, _ := s.sandboxProviders.GetProviderDefinition(providerType)
	return definition
}
