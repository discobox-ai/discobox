package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/obot-platform/discobox/server/internal/model"
)

// PlatformDefaultProvider returns the default provider type for the OS.
func PlatformDefaultProvider() string {
	if runtime.GOOS == "darwin" {
		return "vz"
	}
	if runtime.GOOS == "windows" {
		return "hcs"
	}
	return "docker"
}

// ProviderFactory builds a provider from a saved provider instance.
type ProviderFactory func(ctx context.Context, instance *model.SandboxProviderInstance) (Provider, error)

// ProviderConfigValidator validates persisted provider-instance configuration.
type ProviderConfigValidator func(config json.RawMessage) error

// ProviderConfigValidatorProvider can validate provider-instance configuration.
type ProviderConfigValidatorProvider interface {
	ValidateConfig(config json.RawMessage) error
}

// ProviderManager manages registered providers, definitions, and factories.
type ProviderManager struct {
	mu         sync.RWMutex
	providers  map[string]Provider
	defs       map[string]ProviderDefinition
	factories  map[string]ProviderFactory
	validators map[string]ProviderConfigValidator
	cache      map[string]cachedProvider
	defaultID  string
}

type cachedProvider struct {
	provider  Provider
	updatedAt time.Time
}

// NewProviderManager creates an empty provider manager.
func NewProviderManager() *ProviderManager {
	return &ProviderManager{
		providers:  make(map[string]Provider),
		defs:       make(map[string]ProviderDefinition),
		factories:  make(map[string]ProviderFactory),
		validators: make(map[string]ProviderConfigValidator),
		cache:      make(map[string]cachedProvider),
		defaultID:  PlatformDefaultProvider(),
	}
}

// RegisterProvider registers a process-wide provider.
func (m *ProviderManager) RegisterProvider(id string, provider Provider) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if provider == nil {
		delete(m.providers, id)
		return
	}
	m.providers[id] = provider
	if dp, ok := provider.(DefinitionProvider); ok {
		m.defs[id] = dp.Definition()
	}
	if validator, ok := provider.(ProviderConfigValidatorProvider); ok {
		m.validators[id] = validator.ValidateConfig
	}
}

// RegisterFactory registers a provider-instance factory for a provider type.
func (m *ProviderManager) RegisterFactory(providerType string, factory ProviderFactory) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if factory == nil {
		delete(m.factories, providerType)
		return
	}
	m.factories[providerType] = factory
}

// RegisterProviderConfigValidator registers config validation for a provider type.
func (m *ProviderManager) RegisterProviderConfigValidator(providerType string, validator ProviderConfigValidator) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if validator == nil {
		delete(m.validators, providerType)
		return
	}
	m.validators[providerType] = validator
}

// RegisterProviderDefinition registers provider metadata without an implementation.
func (m *ProviderManager) RegisterProviderDefinition(id string, definition ProviderDefinition) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.defs[id] = definition
}

// SetDefault sets the default provider ID.
func (m *ProviderManager) SetDefault(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.defaultID = id
}

// DefaultProviderName returns the configured default provider ID.
func (m *ProviderManager) DefaultProviderName() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.defaultID
}

// EnsureDefaultAvailable falls back to the first registered provider when the
// configured default is unavailable.
func (m *ProviderManager) EnsureDefaultAvailable() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.providers[m.defaultID]; ok {
		return true
	}
	for id := range m.providers {
		m.defaultID = id
		return true
	}
	return false
}

// GetProvider returns a registered process-wide provider.
func (m *ProviderManager) GetProvider(id string) (Provider, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if id == "" {
		id = m.defaultID
	}
	provider, ok := m.providers[id]
	if !ok {
		return nil, fmt.Errorf("sandbox provider %q not found", id)
	}
	return provider, nil
}

// GetDefault returns the default provider or nil.
func (m *ProviderManager) GetDefault() Provider {
	provider, _ := m.GetProvider("")
	return provider
}

// ListProviders returns registered provider IDs in stable order.
func (m *ProviderManager) ListProviders() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := make([]string, 0, len(m.providers))
	for id := range m.providers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// GetProviderDefinition returns provider metadata.
func (m *ProviderManager) GetProviderDefinition(id string) (ProviderDefinition, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if def, ok := m.defs[id]; ok {
		return def, true
	}
	if provider, ok := m.providers[id]; ok {
		if dp, ok := provider.(DefinitionProvider); ok {
			return dp.Definition(), true
		}
		return ProviderDefinition{Name: id, Description: "Built-in " + id + " sandbox driver"}, true
	}
	return ProviderDefinition{}, false
}

// ListProviderDefinitions returns all known provider definitions.
func (m *ProviderManager) ListProviderDefinitions() map[string]ProviderDefinition {
	m.mu.RLock()
	defer m.mu.RUnlock()
	defs := make(map[string]ProviderDefinition, len(m.defs)+len(m.providers))
	maps.Copy(defs, m.defs)
	for id, provider := range m.providers {
		if _, ok := defs[id]; ok {
			continue
		}
		if dp, ok := provider.(DefinitionProvider); ok {
			defs[id] = dp.Definition()
			continue
		}
		defs[id] = ProviderDefinition{Name: id, Description: "Built-in " + id + " sandbox driver"}
	}
	return defs
}

// GetProviderStatus returns a provider's status and capability flags.
func (m *ProviderManager) GetProviderStatus(id string) (ProviderStatus, bool) {
	m.mu.RLock()
	provider, ok := m.providers[id]
	m.mu.RUnlock()
	if !ok {
		return ProviderStatus{}, false
	}
	return statusFor(provider), true
}

// ListProviderStatuses returns statuses for all registered providers.
func (m *ProviderManager) ListProviderStatuses() map[string]ProviderStatus {
	providers := m.snapshotProviders()
	statuses := make(map[string]ProviderStatus, len(providers))
	for id, provider := range providers {
		statuses[id] = statusFor(provider)
	}
	return statuses
}

// ValidateProviderConfig validates config for a registered provider type.
func (m *ProviderManager) ValidateProviderConfig(providerType string, config json.RawMessage) error {
	m.mu.RLock()
	validator := m.validators[providerType]
	m.mu.RUnlock()
	if validator == nil {
		return nil
	}
	return validator(config)
}

// ResolveForSandbox returns the provider for a sandbox record.
func (m *ProviderManager) ResolveForSandbox(ctx context.Context, sandbox *model.Sandbox) (Provider, error) {
	if sandbox == nil {
		return nil, fmt.Errorf("sandbox is nil")
	}
	if sandbox.ProviderInstance != nil {
		return m.ResolveInstance(ctx, sandbox.ProviderInstance)
	}
	if sandbox.ProviderInstanceID != nil && strings.TrimSpace(*sandbox.ProviderInstanceID) != "" {
		return m.GetProvider(strings.TrimSpace(*sandbox.ProviderInstanceID))
	}
	return m.GetProvider("")
}

// ResolveInstance returns the provider for a configured provider instance.
func (m *ProviderManager) ResolveInstance(ctx context.Context, instance *model.SandboxProviderInstance) (Provider, error) {
	if instance == nil {
		return m.GetProvider("")
	}
	if instance.Disabled {
		return nil, fmt.Errorf("sandbox provider instance %q is disabled", instance.ID)
	}
	if factory := m.factory(instance.Type); factory != nil {
		return m.cachedProvider(ctx, instance, factory)
	}
	return m.GetProvider(instance.Type)
}

// ListRuntimeSandboxes returns runtime sandboxes from all process-wide providers.
//
// Providers that fail are skipped from the result and included in the returned
// joined error. Callers that can tolerate partial results may use the returned
// sandboxes even when err is non-nil.
func (m *ProviderManager) ListRuntimeSandboxes(ctx context.Context) ([]*Sandbox, error) {
	var sandboxes []*Sandbox
	var errs []error
	for id, provider := range m.snapshotProviders() {
		providerSandboxes, err := provider.List(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("list sandbox provider %q: %w", id, err))
			continue
		}
		sandboxes = append(sandboxes, providerSandboxes...)
	}
	return sandboxes, errors.Join(errs...)
}

// ReconcileProviders runs startup reconciliation on all process-wide providers.
func (m *ProviderManager) ReconcileProviders(ctx context.Context) error {
	var errs []error
	for id, provider := range m.snapshotProviders() {
		reconciler, ok := provider.(ReconcileProvider)
		if !ok {
			continue
		}
		if err := reconciler.Reconcile(ctx); err != nil {
			errs = append(errs, fmt.Errorf("reconcile sandbox provider %q: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

// RemoveProjectResources removes provider-managed resources for a project
// across all process-wide providers.
func (m *ProviderManager) RemoveProjectResources(ctx context.Context, projectID string) error {
	var errs []error
	for id, provider := range m.snapshotProviders() {
		remover, ok := provider.(ProjectRemover)
		if !ok {
			continue
		}
		if err := remover.RemoveProject(ctx, projectID); err != nil {
			errs = append(errs, fmt.Errorf("remove project resources from sandbox provider %q: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

// Shutdown closes registered providers that implement Close.
func (m *ProviderManager) Shutdown() {
	for _, provider := range m.snapshotProviders() {
		if closer, ok := provider.(interface{ Close() error }); ok {
			_ = closer.Close()
		}
	}
}

func (m *ProviderManager) factory(providerType string) ProviderFactory {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.factories[providerType]
}

func (m *ProviderManager) cachedProvider(ctx context.Context, instance *model.SandboxProviderInstance, factory ProviderFactory) (Provider, error) {
	m.mu.RLock()
	if cached, ok := m.cache[instance.ID]; ok && cached.updatedAt.Equal(instance.UpdatedAt) {
		provider := cached.provider
		m.mu.RUnlock()
		return provider, nil
	}
	m.mu.RUnlock()

	provider, err := factory(ctx, instance)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.cache[instance.ID] = cachedProvider{provider: provider, updatedAt: instance.UpdatedAt}
	m.mu.Unlock()
	return provider, nil
}

func (m *ProviderManager) snapshotProviders() map[string]Provider {
	m.mu.RLock()
	defer m.mu.RUnlock()
	providers := make(map[string]Provider, len(m.providers))
	maps.Copy(providers, m.providers)
	return providers
}

func statusFor(provider Provider) ProviderStatus {
	status := ProviderStatus{Available: true, State: "ready"}
	if sp, ok := provider.(StatusProvider); ok {
		status = sp.Status()
	}
	_, status.SupportsResources = provider.(ProviderResourceManager)
	_, status.SupportsInspection = provider.(ProjectInspectionManager)
	_, status.SupportsClearCache = provider.(ProjectCacheManager)
	_, status.SupportsImages = provider.(ImageProvider)
	return status
}
