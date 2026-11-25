package pools

import (
	"net/http"

	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/model"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	services "github.com/discobox-ai/discobox/server/internal/services"
)

// sizeOf lists every size field a create request carries, zero or not.
func sizeOf(input services.CreatePoolBody) []sandbox.SizeValue {
	return []sandbox.SizeValue{
		{Field: sandbox.SizeCPU, Value: input.CpuVcpus.Or(0)},
		{Field: sandbox.SizeMemory, Value: float64(input.MemoryBytes.Or(0))},
		{Field: sandbox.SizeStorage, Value: float64(input.StorageBytes.Or(0))},
	}
}

// updatedSizeOf lists only the size fields an update request names.
// A field it leaves out keeps its stored value and is not this request's to
// judge, so a rename of a pool never fails over a size set long ago.
func updatedSizeOf(input services.UpdatePoolBody) []sandbox.SizeValue {
	var values []sandbox.SizeValue
	if value, ok := input.CpuVcpus.Get(); ok {
		values = append(values, sandbox.SizeValue{Field: sandbox.SizeCPU, Value: value})
	}
	if value, ok := input.MemoryBytes.Get(); ok {
		values = append(values, sandbox.SizeValue{Field: sandbox.SizeMemory, Value: float64(value)})
	}
	if value, ok := input.StorageBytes.Get(); ok {
		values = append(values, sandbox.SizeValue{Field: sandbox.SizeStorage, Value: float64(value)})
	}
	return values
}

// checkPoolSize refuses size values the pool's provider would not act
// on, so that a pool setting is never accepted and then silently ignored.
//
// The pool service decides nothing about backends here. Whether a provider
// acts on a field is what its ProviderDefinition declares (SizeFields), next
// to the code that honors it; this only holds every request to that
// declaration, whatever the provider type.
func (s *Service) checkPoolSize(provider *model.SandboxProviderInstance, values []sandbox.SizeValue) error {
	if err := s.providerDefinition(provider.Type).CheckSize("pool", provider.Name, provider.Type, values); err != nil {
		return apperrors.NewStatusError(http.StatusBadRequest, err.Error())
	}
	return nil
}

// providerDefinition returns what the provider type declares. A type this
// server has no definition for declares nothing, which is correct: a provider
// that is not registered here acts on nothing here.
func (s *Service) providerDefinition(providerType string) sandbox.ProviderDefinition {
	if s.providers == nil {
		return sandbox.ProviderDefinition{}
	}
	definition, _ := s.providers.GetProviderDefinition(providerType)
	return definition
}
