package store

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/secrets"
)

// providerSecretsPurpose names the sealed half of a provider instance's
// config (ADR 26-10-31-694 §1): the fields its definition types secret.
const providerSecretsPurpose = "sandbox_provider_instances.encrypted_config"

func providerSecretsResourceID(provider *model.SandboxProviderInstance) string {
	return provider.ProjectID + "/" + provider.ID
}

func (s *Store) ListSandboxProviderInstances(ctx context.Context, projectID string) ([]model.SandboxProviderInstance, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var providers []model.SandboxProviderInstance
	err = read.
		Where("project_id = ?", projectID).
		Order("created_at ASC").
		Find(&providers).Error
	return providers, err
}

// CreateSandboxProviderInstance writes a new provider instance, sealing its
// EncryptedConfig when it holds the secret fields in the clear.
func (s *Store) CreateSandboxProviderInstance(ctx context.Context, provider *model.SandboxProviderInstance) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	if provider.ID == "" {
		if err := provider.BeforeCreate(nil); err != nil {
			return err
		}
	}
	if err := s.sealProviderSecrets(ctx, provider); err != nil {
		return err
	}
	return write.Create(provider).Error
}

func (s *Store) GetSandboxProviderInstance(ctx context.Context, projectID, providerID string) (*model.SandboxProviderInstance, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	return firstByID[model.SandboxProviderInstance](read.Where("project_id = ?", projectID), "id", providerID)
}

// LockSandboxProviderInstance reads a provider instance locked for update, for
// a transaction that decides from the row what it writes over it: an update
// keeps the sealed fields it reads, and two updates that each read them
// unlocked could write a replaced key back (store/REVIEW.md). Call it on the
// store a Store.Transaction callback was handed.
func (s *Store) LockSandboxProviderInstance(ctx context.Context, projectID, providerID string) (*model.SandboxProviderInstance, error) {
	write, err := s.getWrite(ctx)
	if err != nil {
		return nil, err
	}
	return firstByID[model.SandboxProviderInstance](write.Clauses(clause.Locking{Strength: "UPDATE"}).Where("project_id = ?", projectID), "id", providerID)
}

// UpdateSandboxProviderInstance writes provider, sealing its EncryptedConfig
// when it holds the secret fields in the clear.
func (s *Store) UpdateSandboxProviderInstance(ctx context.Context, provider *model.SandboxProviderInstance) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	if err := s.sealProviderSecrets(ctx, provider); err != nil {
		return err
	}
	return write.Save(provider).Error
}

func (s *Store) DeleteSandboxProviderInstance(ctx context.Context, projectID, providerID string) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	provider, err := firstByID[model.SandboxProviderInstance](write.Where("project_id = ?", projectID), "id", providerID)
	if err != nil {
		return err
	}
	return write.Delete(provider).Error
}

// SetSandboxProviderSecrets puts fields in provider's EncryptedConfig, in the
// clear, for the next create or update of it to seal. No fields empties it.
func SetSandboxProviderSecrets(provider *model.SandboxProviderInstance, fields map[string]json.RawMessage) error {
	if len(fields) == 0 {
		provider.EncryptedConfig = nil
		return nil
	}
	data, err := json.Marshal(fields)
	if err != nil {
		return fmt.Errorf("encode provider secret fields: %w", err)
	}
	provider.EncryptedConfig = data
	return nil
}

// OpenSandboxProviderSecrets opens a provider instance's sealed config fields:
// the values of the fields its definition types secret, by key. It returns
// nil when there are none, and does not modify provider.
func (s *Store) OpenSandboxProviderSecrets(ctx context.Context, provider *model.SandboxProviderInstance) (map[string]json.RawMessage, error) {
	if provider == nil || len(provider.EncryptedConfig) == 0 {
		return nil, nil
	}
	plaintext := provider.EncryptedConfig
	// A server with no sealer keeps them as it keeps a secret's value: in the
	// clear.
	if s.sealer != nil && secrets.IsSealed(provider.EncryptedConfig) {
		opened, err := secrets.Open(ctx, s.sealer, providerSecretsPurpose, providerSecretsResourceID(provider), provider.EncryptedConfig)
		if err != nil {
			return nil, fmt.Errorf("decrypt provider %s secret fields: %w", provider.ID, err)
		}
		plaintext = opened
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(plaintext, &fields); err != nil {
		return nil, fmt.Errorf("decode provider %s secret fields: %w", provider.ID, err)
	}
	return fields, nil
}

func (s *Store) sealProviderSecrets(ctx context.Context, provider *model.SandboxProviderInstance) error {
	sealed, err := secrets.SealIfUnsealed(ctx, s.sealer, providerSecretsPurpose, providerSecretsResourceID(provider), provider.EncryptedConfig)
	if err != nil {
		return fmt.Errorf("encrypt provider %s secret fields: %w", provider.ID, err)
	}
	provider.EncryptedConfig = sealed
	return nil
}

// storedProviderCredential is one config field a provider type stored a
// credential in before ADR 26-10-31-694, and the environment references that
// named the process variable holding it instead.
type storedProviderCredential struct {
	key string
	// envKey is the field that named an environment variable in place of the
	// value, if the type had one.
	envKey string
}

// storedProviderCredentials are the credentials SealProviderCredentials moves,
// by provider type. They are what was stored before the decision, written
// out here rather than read from the provider definitions, so that a later
// definition cannot change what the migration moves (ADR 26-10-31-694 §4).
//
// The keys are the types' stored type strings — the ProviderType constants —
// not their package names: the exec provider's package is execvm, its type
// "exec".
var storedProviderCredentials = map[string][]storedProviderCredential{
	"digitalocean": {
		{key: "token", envKey: "tokenEnv"},
		{key: "sshPrivateKey", envKey: "sshPrivateKeyEnv"},
	},
	"exec": {
		{key: "sshPrivateKey", envKey: "sshPrivateKeyEnv"},
	},
	"vm": {
		{key: "apiKey"},
	},
}

// SealProviderCredentials moves the credentials a provider instance's config
// held in the clear into its sealed EncryptedConfig (ADR 26-10-31-694 §4).
//
// It runs at every start, before the instances are resolved, and leaves a row
// with nothing left to move alone. Each instance moves in its own
// transaction. A non-empty value is sealed, merged over what is sealed
// already. An environment reference is resolved here, once, through
// lookupEnv: a variable that is set is sealed as a value would be, and either
// way the reference goes, so the provider resolves with its key or fails
// naming the field to set. No instance is deleted or recreated. An instance
// that cannot be moved is logged and left; only a store error fails it.
func (s *Store) SealProviderCredentials(ctx context.Context, lookupEnv func(string) (string, bool)) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	var types []string
	for providerType := range storedProviderCredentials {
		types = append(types, providerType)
	}
	var ids []model.SandboxProviderInstance
	if err := write.Select("id", "project_id").Where("type IN ?", types).Find(&ids).Error; err != nil {
		return err
	}
	for _, row := range ids {
		if err := s.Transaction(ctx, func(txStore *Store, tx *gorm.DB) error {
			var provider model.SandboxProviderInstance
			if err := tx.Where("project_id = ? AND id = ?", row.ProjectID, row.ID).First(&provider).Error; err != nil {
				return err
			}
			return txStore.sealProviderCredentials(ctx, tx, &provider, lookupEnv)
		}); err != nil {
			// One instance's row does not stop the server, as one that
			// does not resolve does not: what is left in the clear fails
			// its resolve instead, and never reaches its provider.
			slog.ErrorContext(ctx, "provider credentials were not sealed", "project", row.ProjectID, "provider", row.ID, "error", err)
		}
	}
	return nil
}

func (s *Store) sealProviderCredentials(ctx context.Context, tx *gorm.DB, provider *model.SandboxProviderInstance, lookupEnv func(string) (string, bool)) error {
	var config map[string]json.RawMessage
	if len(provider.Config) > 0 {
		if err := json.Unmarshal(provider.Config, &config); err != nil {
			return fmt.Errorf("decode config: %w", err)
		}
	}
	sealed, err := s.OpenSandboxProviderSecrets(ctx, provider)
	if err != nil {
		return err
	}
	moved := maps.Clone(sealed)
	if moved == nil {
		moved = map[string]json.RawMessage{}
	}
	changed := false
	for _, credential := range storedProviderCredentials[provider.Type] {
		value, hasValue := config[credential.key]
		envName, hasEnv := "", false
		if credential.envKey != "" {
			var raw json.RawMessage
			if raw, hasEnv = config[credential.envKey]; hasEnv {
				_ = json.Unmarshal(raw, &envName)
			}
		}
		if hasValue {
			delete(config, credential.key)
			changed = true
		}
		if hasEnv {
			delete(config, credential.envKey)
			changed = true
		}
		// The value moves as it was stored, byte for byte: the provider
		// trims it as it did before.
		if stringField(value) != "" {
			moved[credential.key] = value
			continue
		}
		if _, ok := moved[credential.key]; ok {
			continue
		}
		name := strings.TrimSpace(envName)
		if name == "" {
			continue
		}
		if fromEnv, ok := lookupEnv(name); ok && strings.TrimSpace(fromEnv) != "" {
			moved[credential.key] = mustJSONString(strings.TrimSpace(fromEnv))
			changed = true
		}
	}
	if !changed {
		return nil
	}
	if len(config) == 0 {
		provider.Config = nil
	} else {
		data, err := json.Marshal(config)
		if err != nil {
			return fmt.Errorf("encode config: %w", err)
		}
		provider.Config = data
	}
	if err := SetSandboxProviderSecrets(provider, moved); err != nil {
		return err
	}
	if err := s.sealProviderSecrets(ctx, provider); err != nil {
		return err
	}
	return tx.Save(provider).Error
}

// stringField is a config value's string, trimmed, or empty when it is not a
// string.
func stringField(raw json.RawMessage) string {
	var value string
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return strings.TrimSpace(value)
}

func mustJSONString(value string) json.RawMessage {
	data, _ := json.Marshal(value)
	return data
}
