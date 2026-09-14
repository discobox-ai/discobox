package pools

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/model"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	services "github.com/discobox-ai/discobox/server/internal/services"
)

// poolSizeValue is one size field as a request sets it.
type poolSizeValue struct {
	field sandbox.PoolSizeField
	value float64
}

// sizeOf lists every size field a create request carries, zero or not.
func sizeOf(input services.CreatePoolBody) []poolSizeValue {
	return []poolSizeValue{
		{sandbox.PoolSizeCPU, input.CpuVcpus.Or(0)},
		{sandbox.PoolSizeMemory, float64(input.MemoryBytes.Or(0))},
		{sandbox.PoolSizeStorage, float64(input.StorageBytes.Or(0))},
	}
}

// updatedSizeOf lists only the size fields an update request names.
// A field it leaves out keeps its stored value and is not this request's to
// judge, so a rename of a pool never fails over a size set long ago.
func updatedSizeOf(input services.UpdatePoolBody) []poolSizeValue {
	var values []poolSizeValue
	if value, ok := input.CpuVcpus.Get(); ok {
		values = append(values, poolSizeValue{sandbox.PoolSizeCPU, value})
	}
	if value, ok := input.MemoryBytes.Get(); ok {
		values = append(values, poolSizeValue{sandbox.PoolSizeMemory, float64(value)})
	}
	if value, ok := input.StorageBytes.Get(); ok {
		values = append(values, poolSizeValue{sandbox.PoolSizeStorage, float64(value)})
	}
	return values
}

// checkPoolSize refuses size values the pool's provider would not act
// on, so that a pool setting is never accepted and then silently ignored.
//
// The pool service decides nothing about backends here. Whether a provider
// acts on a field is what its ProviderDefinition declares (PoolSizeFields), next
// to the code that honors it; this only holds every request to that
// declaration, whatever the provider type.
//
// Zero is always accepted: it is "unset", and clearing a value no
// provider ever acted on has to stay possible. A negative value is refused on
// every provider, because no backend reads one and it would do nothing.
func (s *Service) checkPoolSize(provider *model.SandboxProviderInstance, values []poolSizeValue) error {
	var requested []sandbox.PoolSizeField
	for _, v := range values {
		if v.value < 0 {
			return apperrors.NewStatusError(http.StatusBadRequest, fmt.Sprintf("pool %s must not be negative", v.field))
		}
		if v.value > 0 {
			requested = append(requested, v.field)
		}
	}
	if len(requested) == 0 {
		return nil
	}

	definition := s.providerDefinition(provider.Type)
	var refused []string
	for _, field := range requested {
		if !definition.ActsOnPoolSize(field) {
			refused = append(refused, string(field))
		}
	}
	if len(refused) == 0 {
		return nil
	}

	accepted := "takes no pool size settings"
	if len(definition.PoolSizeFields) > 0 {
		names := make([]string, len(definition.PoolSizeFields))
		for i, field := range definition.PoolSizeFields {
			names[i] = string(field)
		}
		accepted = "acts only on " + strings.Join(names, ", ")
	}
	return apperrors.NewStatusError(http.StatusBadRequest, fmt.Sprintf(
		"pool %s would have no effect on provider instance %q: the %s provider %s; leave it unset (0)",
		strings.Join(refused, ", "), provider.Name, provider.Type, accepted))
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
