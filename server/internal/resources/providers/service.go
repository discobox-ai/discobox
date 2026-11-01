package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"

	"gorm.io/gorm"

	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/resources/pools"
	sandboxesvc "github.com/discobox-ai/discobox/server/internal/resources/sandboxes"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

type Service struct {
	store     *store.Store
	sandboxes SandboxCatalogService
	pools     *pools.ControlPlane
}

type SandboxCatalogService interface {
	ListSandboxProviderCatalog() []SandboxProviderCatalogItem
	SandboxProviderManager() *sandbox.ProviderManager
}

type SandboxProviderCatalogItem = sandboxesvc.SandboxProviderCatalogItem

func NewService(store *store.Store, sandboxes SandboxCatalogService, poolManager *pools.ControlPlane) *Service {
	return &Service{store: store, sandboxes: sandboxes, pools: poolManager}
}

func (s *Service) ListSandboxProviderCatalogItems(context.Context) ([]services.SandboxProviderCatalogItem, error) {
	items := s.sandboxes.ListSandboxProviderCatalog()
	out := make([]services.SandboxProviderCatalogItem, 0, len(items))
	for _, item := range items {
		out = append(out, providerCatalogItemToService(item))
	}
	return out, nil
}

func providerCatalogItemToService(item SandboxProviderCatalogItem) services.SandboxProviderCatalogItem {
	out := services.SandboxProviderCatalogItem{
		ID:           item.ID,
		Name:         item.Name,
		Capabilities: providerStatusToService(item.Capabilities),
	}
	if item.Icon != "" {
		out.Icon = services.OptString{Value: item.Icon, Set: true}
	}
	if item.Description != "" {
		out.Description = services.OptString{Value: item.Description, Set: true}
	}
	if item.ConfigFields != nil {
		fields := make([]services.ProviderConfigField, 0, len(item.ConfigFields))
		for _, field := range item.ConfigFields {
			fields = append(fields, providerConfigFieldToService(field))
		}
		out.ConfigFields = services.OptNilProviderConfigFieldArray{Value: fields, Set: true}
	}
	return out
}

func providerConfigFieldToService(field sandboxesvc.ProviderConfigField) services.ProviderConfigField {
	out := services.ProviderConfigField{
		Key:   field.Key,
		Label: field.Label,
		Type:  field.Type,
	}
	if field.Description != "" {
		out.Description = services.OptString{Value: field.Description, Set: true}
	}
	if field.Placeholder != "" {
		out.Placeholder = services.OptString{Value: field.Placeholder, Set: true}
	}
	if field.Required {
		out.Required = services.OptBool{Value: field.Required, Set: true}
	}
	if field.Advanced {
		out.Advanced = services.OptBool{Value: field.Advanced, Set: true}
	}
	if field.Immutable {
		out.Immutable = services.OptBool{Value: field.Immutable, Set: true}
	}
	return out
}

func providerStatusToService(status sandboxesvc.ProviderStatus) services.ProviderStatus {
	out := services.ProviderStatus{
		Available: status.Available,
		State:     status.State,
	}
	if status.Message != "" {
		out.Message = services.OptString{Value: status.Message, Set: true}
	}
	if status.Details != nil {
		if data, err := json.Marshal(status.Details); err == nil {
			out.Details = data
		}
	}
	return out
}

func (s *Service) ListSandboxProviderInstances(ctx context.Context, projectID string) ([]model.SandboxProviderInstance, error) {
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		return nil, apperrors.NotFound(err, "project not found")
	}
	providers, err := s.store.ListSandboxProviderInstances(ctx, projectID)
	if err != nil {
		return nil, err
	}
	for i := range providers {
		s.readSecretFields(ctx, &providers[i])
	}
	return providers, nil
}

func (s *Service) CreateSandboxProviderInstance(ctx context.Context, projectID string, input services.CreateSandboxProviderInstanceBody) (*model.SandboxProviderInstance, error) {
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		return nil, apperrors.NotFound(err, "project not found")
	}
	if strings.TrimSpace(input.Type) == "" {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "type is required")
	}
	manager := s.sandboxes.SandboxProviderManager()
	config := services.RawMessage(input.Config)
	if err := manager.ValidateProviderConfig(input.Type, config); err != nil {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, err.Error())
	}
	plain, sealed, err := s.splitConfig(input.Type, config, nil)
	if err != nil {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, err.Error())
	}
	provider := &model.SandboxProviderInstance{ProjectID: projectID, Type: input.Type, Name: input.Name, Config: model.RawJSON(plain)}
	if err := store.SetSandboxProviderSecrets(provider, sealed); err != nil {
		return nil, err
	}
	if err := s.store.CreateSandboxProviderInstance(ctx, provider); err != nil {
		return nil, err
	}
	// The row is written first because resolving keys the provider cache on its
	// ID and runs provider I/O that must not hold the write transaction. A
	// create that fails leaves nothing behind, so the row goes again, even when
	// the resolve failed because the request's context ended. The config has
	// passed validation, so a resolve that fails is the backend not coming up:
	// a 502, not the caller's input.
	if _, err := manager.ResolveInstance(ctx, provider); err != nil {
		resolveErr := apperrors.NewStatusError(http.StatusBadGateway, err.Error())
		if deleteErr := s.store.DeleteSandboxProviderInstance(context.WithoutCancel(ctx), projectID, provider.ID); deleteErr != nil {
			return nil, errors.Join(resolveErr, fmt.Errorf("remove provider instance %s that did not resolve: %w", provider.ID, deleteErr))
		}
		return nil, resolveErr
	}
	return s.getWithSecretFields(ctx, projectID, provider.ID)
}

func (s *Service) GetSandboxProviderInstance(ctx context.Context, projectID, providerID string) (*model.SandboxProviderInstance, error) {
	provider, err := s.getWithSecretFields(ctx, projectID, providerID)
	if err != nil {
		return nil, apperrors.NotFound(err, "provider instance not found")
	}
	return provider, nil
}

// getWithSecretFields reads a provider instance as the API answers with it:
// its config, which holds no secret field, and the names of the secret fields
// that have a value.
func (s *Service) getWithSecretFields(ctx context.Context, projectID, providerID string) (*model.SandboxProviderInstance, error) {
	provider, err := s.store.GetSandboxProviderInstance(ctx, projectID, providerID)
	if err != nil {
		return nil, err
	}
	s.readSecretFields(ctx, provider)
	return provider, nil
}

// readSecretFields makes provider what a read answers with (ADR 26-10-31-694
// §2): its config without any secret field, and SecretFields, the secret
// fields its row seals a value for. A secret field still in the stored config
// — a row the startup migration could not move, or one an older server wrote
// — is taken out of the answer as well; it fails the instance's resolve.
//
// A row whose sealed fields do not open is that instance's problem, as one
// that does not resolve is: it is logged and answered with no secretFields,
// so that the listing, and the update or delete that would mend it, still
// work.
func (s *Service) readSecretFields(ctx context.Context, provider *model.SandboxProviderInstance) {
	definition, _ := s.sandboxes.SandboxProviderManager().GetProviderDefinition(provider.Type)
	provider.Config = model.RawJSON(withoutSecretFields(json.RawMessage(provider.Config), definition.SecretFields()))
	sealed, err := s.store.OpenSandboxProviderSecrets(ctx, provider)
	if err != nil {
		slog.ErrorContext(ctx, "provider instance's secret fields did not open", "project", provider.ProjectID, "provider", provider.ID, "error", err)
		return
	}
	provider.SecretFields = slices.Sorted(maps.Keys(sealed))
}

// withoutSecretFields is config with the secret keys taken out. A config that
// is not an object, or holds none of them, is returned as it is.
func withoutSecretFields(config json.RawMessage, secretKeys []string) json.RawMessage {
	var fields map[string]json.RawMessage
	if len(secretKeys) == 0 || len(config) == 0 || json.Unmarshal(config, &fields) != nil {
		return config
	}
	removed := false
	for _, key := range secretKeys {
		if _, ok := fields[key]; ok {
			delete(fields, key)
			removed = true
		}
	}
	if !removed {
		return config
	}
	if len(fields) == 0 {
		return nil
	}
	data, err := json.Marshal(fields)
	if err != nil {
		return nil
	}
	return data
}

// splitConfig splits a config a caller wrote into the config stored in the
// clear and the secret fields sealed apart from it, by the secret fields the
// provider's definition declares (ADR 26-10-31-694 §§1, 2). kept is what is
// sealed already: a secret field the config leaves out keeps its value there,
// a value replaces it, and null clears it. Only an object
// can be split; an empty config is an empty object.
func (s *Service) splitConfig(providerType string, config json.RawMessage, kept map[string]json.RawMessage) (json.RawMessage, map[string]json.RawMessage, error) {
	sealed := maps.Clone(kept)
	if sealed == nil {
		sealed = map[string]json.RawMessage{}
	}
	if len(bytes.TrimSpace(config)) == 0 {
		return nil, sealed, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(config, &fields); err != nil {
		return nil, nil, fmt.Errorf("provider config must be a JSON object: %w", err)
	}
	definition, _ := s.sandboxes.SandboxProviderManager().GetProviderDefinition(providerType)
	secretKeys := definition.SecretFields()
	if !slices.ContainsFunc(secretKeys, func(key string) bool { _, ok := fields[key]; return ok }) {
		return config, sealed, nil
	}
	for _, key := range secretKeys {
		value, ok := fields[key]
		if !ok {
			continue
		}
		delete(fields, key)
		if clearsSecretField(value) {
			delete(sealed, key)
			continue
		}
		sealed[key] = value
	}
	if len(fields) == 0 {
		return nil, sealed, nil
	}
	plain, err := json.Marshal(fields)
	if err != nil {
		return nil, nil, err
	}
	return plain, sealed, nil
}

// clearsSecretField reports whether a secret field's value in a write clears
// it: null. Any other value replaces it.
func clearsSecretField(value json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(value), []byte("null"))
}

func (s *Service) UpdateSandboxProviderInstance(ctx context.Context, projectID, providerID string, input services.UpdateSandboxProviderInstanceBody) (*model.SandboxProviderInstance, error) {
	// The row is read locked and written back whole in one transaction: the
	// sealed fields an update keeps are read from it, and a read on the root
	// store, which may be a lagging replica, or an unlocked read beside a
	// concurrent rotation, would write back a key it had just replaced
	// (store/REVIEW.md).
	err := s.store.Transaction(ctx, func(tx *store.Store, _ *gorm.DB) error {
		provider, err := tx.LockSandboxProviderInstance(ctx, projectID, providerID)
		if err != nil {
			return apperrors.NotFound(err, "provider instance not found")
		}
		if name, ok := input.Name.Get(); ok {
			provider.Name = name
		}
		if len(input.Config) > 0 {
			if err := s.updateConfig(ctx, tx, provider, services.RawMessage(input.Config)); err != nil {
				return err
			}
		}
		if disabled, ok := input.Disabled.Get(); ok {
			provider.Disabled = disabled
		}
		return tx.UpdateSandboxProviderInstance(ctx, provider)
	})
	if err != nil {
		return nil, err
	}
	return s.getWithSecretFields(ctx, projectID, providerID)
}

// updateConfig replaces provider's config with config, keeping each secret
// field config leaves out (ADR 26-10-31-694 §2).
func (s *Service) updateConfig(ctx context.Context, tx *store.Store, provider *model.SandboxProviderInstance, config json.RawMessage) error {
	kept, err := tx.OpenSandboxProviderSecrets(ctx, provider)
	if err != nil {
		return err
	}
	plain, sealed, err := s.splitConfig(provider.Type, config, kept)
	if err != nil {
		return apperrors.NewStatusError(http.StatusBadRequest, err.Error())
	}
	// Validated whole, with the secret fields it keeps, as a create is.
	joined, err := sandbox.JoinProviderConfig(plain, sealed)
	if err != nil {
		return apperrors.NewStatusError(http.StatusBadRequest, err.Error())
	}
	if err := s.sandboxes.SandboxProviderManager().ValidateProviderConfig(provider.Type, joined); err != nil {
		return apperrors.NewStatusError(http.StatusBadRequest, err.Error())
	}
	// No secret field is immutable: a key changes while pools stand, which
	// is what rotating it is.
	if err := s.refuseImmutableChange(ctx, tx, provider, plain); err != nil {
		return err
	}
	provider.Config = model.RawJSON(plain)
	return store.SetSandboxProviderSecrets(provider, sealed)
}

// refuseImmutableChange refuses a config that changes a field the provider
// declares immutable while the instance has pools: their hosts were made by
// what the field names, and nothing under the new value could remove them. It
// is the update's counterpart of the delete refusal below, and like it allows
// anything once the pools are gone.
func (s *Service) refuseImmutableChange(ctx context.Context, tx *store.Store, provider *model.SandboxProviderInstance, config json.RawMessage) error {
	definition, ok := s.sandboxes.SandboxProviderManager().GetProviderDefinition(provider.Type)
	if !ok {
		return nil
	}
	var changed []string
	for _, field := range definition.ConfigFields {
		if !field.Immutable {
			continue
		}
		before, err := configValue(provider.Config, field.Key)
		if err != nil {
			return err
		}
		after, err := configValue(config, field.Key)
		if err != nil {
			return err
		}
		if before != after {
			changed = append(changed, field.Key)
		}
	}
	if len(changed) == 0 {
		return nil
	}
	pools, err := tx.ListPoolsForProviderInstance(ctx, provider.ProjectID, provider.ID)
	if err != nil {
		return err
	}
	if len(pools) > 0 {
		return apperrors.NewStatusError(http.StatusConflict, fmt.Sprintf("provider instance has pools, so %s cannot change: delete its pools first", strings.Join(changed, ", ")))
	}
	return nil
}

// configValue is one top-level field of a provider config, compacted, so that
// formatting does not read as a change. An absent field is empty.
func configValue(config []byte, key string) (string, error) {
	if len(config) == 0 {
		return "", nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(config, &fields); err != nil {
		return "", fmt.Errorf("decode provider config: %w", err)
	}
	value, ok := fields[key]
	if !ok {
		return "", nil
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, value); err != nil {
		return "", fmt.Errorf("decode provider config %s: %w", key, err)
	}
	return compact.String(), nil
}

func (s *Service) DeleteSandboxProviderInstance(ctx context.Context, projectID, providerID string) error {
	// Pools bind to a provider instance immutably, so a provider instance with
	// pools cannot be deleted; sandboxes hang off the pools.
	pools, err := s.store.ListPoolsForProviderInstance(ctx, projectID, providerID)
	if err != nil {
		return err
	}
	if len(pools) > 0 {
		return apperrors.NewStatusError(http.StatusConflict, "provider instance has pools")
	}
	return apperrors.NotFound(s.store.DeleteSandboxProviderInstance(ctx, projectID, providerID), "provider instance not found")
}

// EnsureExistingSandboxProviderInstances schedules provider startup
// reconciliation for persisted provider instances.
//
// An instance that does not resolve is logged and does not fail the start: it
// belongs to one project, and a server that will not start cannot serve the
// API that would disable or delete it. Its pools are marked instead, so their
// reconciles retry the resolve with backoff and carry the error.
func (s *Service) EnsureExistingSandboxProviderInstances(ctx context.Context) error {
	projects, err := s.store.ListProjects(ctx)
	if err != nil {
		return err
	}
	for i := range projects {
		project := &projects[i]
		providers, err := s.store.ListSandboxProviderInstances(ctx, project.ID)
		if err != nil {
			return err
		}
		for i := range providers {
			provider := &providers[i]
			if provider.Disabled {
				continue
			}
			if _, err := s.sandboxes.SandboxProviderManager().ResolveInstance(ctx, provider); err != nil {
				slog.Error("sandbox provider instance did not resolve", "project", project.ID, "provider", provider.ID, "type", provider.Type, "error", err)
				if err := s.EnqueueProviderPools(ctx, project.ID, provider.ID); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (s *Service) EnqueueProviderPools(ctx context.Context, projectID, providerID string) error {
	poolRows, err := s.store.ListPoolsForProviderInstance(ctx, projectID, providerID)
	if err != nil {
		return err
	}
	for i := range poolRows {
		if err := s.pools.SchedulePoolReconciliation(ctx, projectID, poolRows[i].ID); err != nil {
			return err
		}
	}
	return nil
}
