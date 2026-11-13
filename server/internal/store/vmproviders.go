package store

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"strings"

	"gorm.io/gorm"

	"github.com/discobox-ai/discobox/server/internal/model"
)

// retiredVMProviderType is the provider type every disco-vm driver shared
// before each became a type of its own name (ADR 26-11-11-080 §8).
const retiredVMProviderType = "vm"

// vmDriverUpgrade is how a vm provider on one driver discobox links becomes a
// provider of the driver's own type: the secret field the driver names for
// its key, which the vm type's sealed apiKey becomes, and the server
// environment variables the old pin's driver read its endpoints from, which
// are carried into the row as the driver's fields.
type vmDriverUpgrade struct {
	// secret is the driver's secret field, under driver; empty for a driver
	// that takes no key.
	secret string
	// carried is, by driver field, the server environment variable the old
	// pin read it from.
	carried []vmCarriedEndpoint
}

type vmCarriedEndpoint struct {
	field string
	env   string
}

// vmDriverUpgrades are the drivers whose vm providers are upgraded, written
// out here rather than read from disco-vm's registry, so that a later
// definition cannot change what the migration does (store/REVIEW.md): the
// drivers this release links and their fields at disco-vm cf01befccd92.
var vmDriverUpgrades = map[string]vmDriverUpgrade{
	"boxd": {
		secret: "driver.apiKey",
		carried: []vmCarriedEndpoint{
			{field: "driver.addr", env: "BOXD_GRPC_ADDR"},
			{field: "driver.tokenUrl", env: "BOXD_TOKEN_URL"},
		},
	},
	"archil": {secret: "driver.apiKey"},
	"hcs":    {},
}

// MigrateVMProviders upgrades each provider row still of the retired vm type
// (ADR 26-11-11-080 §8), once: a row of a driver discobox links becomes a
// provider of the driver's type, its sealed apiKey the driver's secret,
// sealed to sealed, and the endpoints the server's environment gave the old
// driver carried into its configuration and marked as the operator's; any
// other row keeps its type and configuration, and is disabled, since its
// pools never ran. The row's ID, and so its disco-vm scope, its pools and
// their machines, keys and records, do not move.
//
// It runs at every start, after SealProviderCredentials, which seals a vm
// row's clear apiKey first, so the key is never left clear; it finds only
// rows still typed vm, so it is idempotent. lookupEnv is read here, and
// nowhere else the server makes a driver. Each row moves in its own
// transaction; one that cannot is logged and left, and only a store error
// fails the start.
func (s *Store) MigrateVMProviders(ctx context.Context, lookupEnv func(string) (string, bool)) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	var rows []model.SandboxProviderInstance
	if err := write.Select("id", "project_id").Where("type = ?", retiredVMProviderType).Find(&rows).Error; err != nil {
		return err
	}
	for _, row := range rows {
		if err := s.Transaction(ctx, func(txStore *Store, tx *gorm.DB) error {
			var provider model.SandboxProviderInstance
			if err := tx.Where("project_id = ? AND id = ? AND type = ?", row.ProjectID, row.ID, retiredVMProviderType).First(&provider).Error; err != nil {
				return err
			}
			return txStore.migrateVMProvider(ctx, tx, &provider, lookupEnv)
		}); err != nil {
			slog.ErrorContext(ctx, "vm provider was not upgraded to its driver's type", "project", row.ProjectID, "provider", row.ID, "error", err)
		}
	}
	return s.dropDeliverProviderKey(ctx)
}

// dropDeliverProviderKey takes deliverProviderKey out of a row already of a
// linked driver's type: one a server between the per-driver types and §9
// moved, which kept it (ADR 26-11-11-080 §9). Rows without it are not
// written.
func (s *Store) dropDeliverProviderKey(ctx context.Context) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	types := make([]string, 0, len(vmDriverUpgrades))
	for driver := range vmDriverUpgrades {
		types = append(types, driver)
	}
	var rows []model.SandboxProviderInstance
	if err := write.Where("type IN ? AND config LIKE ?", types, "%deliverProviderKey%").Find(&rows).Error; err != nil {
		return err
	}
	for i := range rows {
		config := map[string]json.RawMessage{}
		if json.Unmarshal(rows[i].Config, &config) != nil {
			continue
		}
		if _, ok := config["deliverProviderKey"]; !ok {
			continue
		}
		delete(config, "deliverProviderKey")
		data, err := json.Marshal(config)
		if err != nil {
			return err
		}
		if len(config) == 0 {
			data = nil
		}
		if err := write.Model(&model.SandboxProviderInstance{}).Where("id = ?", rows[i].ID).Update("config", model.RawJSON(data)).Error; err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) migrateVMProvider(ctx context.Context, tx *gorm.DB, provider *model.SandboxProviderInstance, lookupEnv func(string) (string, bool)) error {
	config := map[string]json.RawMessage{}
	if len(provider.Config) > 0 {
		if err := json.Unmarshal(provider.Config, &config); err != nil {
			return fmt.Errorf("decode config: %w", err)
		}
	}
	driver := strings.TrimSpace(stringField(config["driver"]))
	upgrade, linked := vmDriverUpgrades[driver]
	if !linked {
		// vz, docker, libkrun, fake, empty or unknown: its pools never ran.
		// The row stays for its owner to delete, and so do its pools.
		if provider.Disabled {
			return nil
		}
		provider.Disabled = true
		slog.WarnContext(ctx, "vm provider disabled: its driver is not a provider type of this server; delete it and its pools", "project", provider.ProjectID, "provider", provider.ID, "driver", driver)
		return tx.Save(provider).Error
	}
	sealed, err := s.OpenSandboxProviderSecrets(ctx, provider)
	if err != nil {
		return err
	}
	moved := maps.Clone(sealed)
	if moved == nil {
		moved = map[string]json.RawMessage{}
	}
	key, hasKey := moved["apiKey"]
	delete(moved, "apiKey")
	// A clear key SealProviderCredentials could not seal on this start is
	// sealed here, under the driver's secret, or the row is not moved: the
	// new type's secret field no longer names apiKey, so nothing would ever
	// seal it, and every read of the provider would return it.
	if clearKey, ok := config["apiKey"]; ok {
		delete(config, "apiKey")
		if !hasKey && stringField(clearKey) != "" {
			key, hasKey = clearKey, true
		}
	}
	if hasKey && upgrade.secret != "" {
		moved[upgrade.secret] = key
	}
	delete(config, "driver")
	// Nothing of the provider's is placed on a pool's machine now (ADR
	// 26-11-11-080 §9): a machine that held the key has it taken off at its
	// next reconcile.
	delete(config, "deliverProviderKey")
	driverConfig := map[string]string{}
	var carried []string
	for _, endpoint := range upgrade.carried {
		value, ok := lookupEnv(endpoint.env)
		if value = strings.TrimSpace(value); !ok || value == "" {
			continue
		}
		driverConfig[strings.TrimPrefix(endpoint.field, "driver.")] = value
		carried = append(carried, endpoint.field)
	}
	if len(driverConfig) > 0 {
		data, err := json.Marshal(driverConfig)
		if err != nil {
			return err
		}
		config["driver"] = data
	}
	provider.Type = driver
	provider.CarriedEndpoints = carried
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
	if len(provider.EncryptedConfig) > 0 {
		if err := s.sealProviderSecrets(ctx, provider); err != nil {
			// The row stays vm, its key where it was, for the next start.
			return err
		}
	}
	slog.InfoContext(ctx, "vm provider upgraded to its driver's type", "project", provider.ProjectID, "provider", provider.ID, "type", driver, "carried", carried)
	return tx.Save(provider).Error
}
