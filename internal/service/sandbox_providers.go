package service

import (
	"sort"

	"github.com/obot-platform/disco2/internal/sandbox"
)

// SandboxProviderCatalogItem describes a registered sandbox provider type.
type SandboxProviderCatalogItem struct {
	ID           string
	Name         string
	Icon         string
	Description  string
	Available    bool
	BuiltIn      bool
	Capabilities sandbox.ProviderStatus
	ConfigFields []sandbox.ProviderConfigField
}

// RegisterSandboxProvider registers a runtime sandbox provider.
func (s *Service) RegisterSandboxProvider(id string, provider sandbox.Provider) {
	if s.sandboxProviders != nil {
		s.sandboxProviders.RegisterProvider(id, provider)
	}
}

// SandboxProviderManager returns the service-owned provider manager.
func (s *Service) SandboxProviderManager() *sandbox.ProviderManager {
	return s.sandboxProviders
}

// NewSandboxOperations returns provider-manager-backed sandbox operations.
func (s *Service) NewSandboxOperations() *SandboxOperations {
	return NewSandboxOperations(
		WithSandboxProviderManager(s.sandboxProviders),
		WithSandboxAuthenticator(s.sandboxAuth),
	)
}

// RegisterSandboxProviderDefinition registers provider metadata without an implementation.
func (s *Service) RegisterSandboxProviderDefinition(id string, definition sandbox.ProviderDefinition) {
	if s.sandboxProviders != nil {
		s.sandboxProviders.RegisterProviderDefinition(id, definition)
	}
}

// ListSandboxProviderNames returns registered runtime provider IDs.
func (s *Service) ListSandboxProviderNames() []string {
	if s.sandboxProviders == nil {
		return nil
	}
	return s.sandboxProviders.ListProviders()
}

// DefaultSandboxProviderName returns the active process default provider ID.
func (s *Service) DefaultSandboxProviderName() string {
	if s.sandboxProviders == nil {
		return ""
	}
	s.sandboxProviders.EnsureDefaultAvailable()
	return s.sandboxProviders.DefaultProviderName()
}

// ListSandboxProviderStatuses returns statuses for registered runtime providers.
func (s *Service) ListSandboxProviderStatuses() map[string]sandbox.ProviderStatus {
	if s.sandboxProviders == nil {
		return nil
	}
	return s.sandboxProviders.ListProviderStatuses()
}

// ListSandboxProviderCatalog returns registered provider definitions with status.
func (s *Service) ListSandboxProviderCatalog() []SandboxProviderCatalogItem {
	if s.sandboxProviders == nil {
		return nil
	}
	statuses := s.sandboxProviders.ListProviderStatuses()
	definitions := s.sandboxProviders.ListProviderDefinitions()
	out := make([]SandboxProviderCatalogItem, 0, len(definitions))
	for id, definition := range definitions {
		status, registered := statuses[id]
		name := definition.Name
		if name == "" {
			name = id
		}
		description := definition.Description
		if description == "" {
			description = "Built-in " + name + " sandbox driver"
		}
		out = append(out, SandboxProviderCatalogItem{
			ID:           id,
			Name:         name,
			Icon:         definition.Icon,
			Description:  description,
			Available:    !registered || status.Available,
			BuiltIn:      registered,
			Capabilities: status,
			ConfigFields: definition.ConfigFields,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
