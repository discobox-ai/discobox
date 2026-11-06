package store

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"gorm.io/gorm"

	"github.com/discobox-ai/discobox/server/internal/encryption"
	"github.com/discobox-ai/discobox/server/internal/model"
)

// EncryptionReport is what PrepareEncryption did.
type EncryptionReport struct {
	// Rewrapped counts data keys wrapped again by the current provider.
	Rewrapped int
	// Resealed counts values sealed again in the current format: values an
	// earlier release sealed, and values stored unsealed.
	Resealed int
	// Unsealed counts values stored unsealed because no provider is
	// configured.
	Unsealed int
}

// resealBatch is how many values one transaction re-seals.
const resealBatch = 100

// sealedColumn is a column the store keeps sealed, and how a row's field is
// named from its key columns.
type sealedColumn struct {
	table  string
	column string
	keys   []string
	field  func(keys []string) encryption.Field
}

var sealedColumns = []sealedColumn{
	{"pools", "proxy_intent", []string{"project_id", "id"}, func(k []string) encryption.Field { return encryption.PoolProxyIntent(k[0], k[1]) }},
	{"secrets", "encrypted_value", []string{"project_id", "id"}, func(k []string) encryption.Field {
		return encryption.SecretValue(k[0], k[1])
	}},
	{"sandboxes", "secret_state", []string{"project_id", "id"}, func(k []string) encryption.Field {
		return encryption.SandboxSecretState(k[0], k[1])
	}},
	{"sandbox_inventories", "document", []string{"project_id", "sandbox_id"}, func(k []string) encryption.Field {
		return encryption.SandboxInventory(k[0], k[1])
	}},
	{"sandbox_provider_instances", "encrypted_config", []string{"project_id", "id"}, func(k []string) encryption.Field {
		return encryption.SandboxProviderSecrets(k[0], k[1])
	}},
	{"sandbox_access_issuer_keys", "encrypted_private_key", []string{"project_id", "user_id"}, func(k []string) encryption.Field {
		return encryption.SandboxAccessIssuerKey(k[0], k[1])
	}},
}

// PrepareEncryption brings what is stored up to the configured provider at
// startup, before anything is served (ADR 26-11-02-902 §§4, 5). It runs on
// every start and does only what is left:
//
//  1. Data keys wrapped by anything but the current provider and key, the
//     previous provider's or a retired local key's, are wrapped again.
//  2. Every project, and the server, gets its data key, outside any
//     transaction (EncryptionKeys).
//  3. Every value an earlier release sealed, or that was stored unsealed, is
//     sealed again, in batches, each in a transaction, so an interrupted
//     run resumes where it stopped.
//
// A value that cannot be opened — sealed by an earlier release when no local
// key holds its key, or sealed when no provider is configured — fails it,
// naming the setting: it is not skipped.
//
// The pool-agent issuer key, which lives in server_state, is re-sealed by
// its own manager.
func (s *Store) PrepareEncryption(ctx context.Context) (EncryptionReport, error) {
	var report EncryptionReport
	write, err := s.getWrite(ctx)
	if err != nil {
		return report, err
	}
	if s.keyring != nil {
		provider, keyID := s.keyring.Provider()
		var scopes []string
		if err := write.Model(&model.EncryptionKey{}).
			Where("provider <> ? OR provider_key_id <> ?", provider, keyID).
			Distinct().Order("scope").Pluck("scope", &scopes).Error; err != nil {
			return report, fmt.Errorf("list data keys to rewrap: %w", err)
		}
		for _, scope := range scopes {
			n, err := s.keyring.Rewrap(ctx, scope)
			report.Rewrapped += n
			if err != nil {
				return report, fmt.Errorf("rewrap the data keys of %s: %w", scope, err)
			}
		}
		var missing []string
		if err := write.Model(&model.Project{}).
			Where("id NOT IN (?)", write.Model(&model.EncryptionKey{}).Select("scope")).
			Order("id").Pluck("id", &missing).Error; err != nil {
			return report, fmt.Errorf("list projects without a data key: %w", err)
		}
		for _, scope := range append(missing, encryption.ServerScope) {
			if err := s.keyring.Ensure(ctx, scope); err != nil {
				return report, fmt.Errorf("make the data key of %s: %w", scope, err)
			}
		}
	}
	for _, col := range sealedColumns {
		if err := s.resealColumn(ctx, write, col, &report); err != nil {
			return report, fmt.Errorf("re-seal %s.%s: %w", col.table, col.column, err)
		}
	}
	return report, nil
}

// head is how much of a value tells its form.
var head = max(len(encryption.LegacyPrefix), len("envelope:v1:"))

// resealColumn re-seals what in col is not sealed in the current format.
// Only each row's key and the head of its value are read to find them.
func (s *Store) resealColumn(ctx context.Context, write *gorm.DB, col sealedColumn, report *EncryptionReport) error {
	keyList := strings.Join(col.keys, ", ")
	rows, err := write.Raw(fmt.Sprintf("SELECT %s, substr(%s, 1, %d) FROM %s WHERE %s IS NOT NULL ORDER BY %s",
		keyList, col.column, head, col.table, col.column, keyList)).Rows()
	if err != nil {
		return err
	}
	var pending [][]string
	for rows.Next() {
		keys := make([]string, len(col.keys))
		var prefix []byte
		dest := make([]any, 0, len(keys)+1)
		for i := range keys {
			dest = append(dest, &keys[i])
		}
		dest = append(dest, &prefix)
		if err := rows.Scan(dest...); err != nil {
			_ = rows.Close()
			return err
		}
		sealed := encryption.Sealed(prefix) || encryption.Legacy(prefix)
		switch {
		case len(prefix) == 0:
		case s.keyring != nil && encryption.Sealed(prefix):
		case s.keyring == nil && !sealed:
			report.Unsealed++
		default:
			pending = append(pending, keys)
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for start := 0; start < len(pending); start += resealBatch {
		batch := pending[start:min(start+resealBatch, len(pending))]
		if err := s.Transaction(ctx, func(txStore *Store, tx *gorm.DB) error {
			for _, keys := range batch {
				if err := txStore.resealValue(ctx, tx, col, keys); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}
		report.Resealed += len(batch)
	}
	return nil
}

func (s *Store) resealValue(ctx context.Context, tx *gorm.DB, col sealedColumn, keys []string) error {
	where := make([]string, len(col.keys))
	args := make([]any, len(keys))
	for i, k := range col.keys {
		where[i] = k + " = ?"
		args[i] = keys[i]
	}
	cond := strings.Join(where, " AND ")
	var value []byte
	if err := tx.Raw(fmt.Sprintf("SELECT %s FROM %s WHERE %s", col.column, col.table, cond), args...).Row().Scan(&value); err != nil {
		return err
	}
	field := col.field(keys)
	if s.keyring == nil {
		// Reached only for a sealed value: it cannot be opened, and storing
		// it unsealed is not this server's to do without a provider.
		return fmt.Errorf("%s %s is sealed, and encryption.provider is not set: configure the provider, or for a value sealed with encryptionKey, encryption.provider: local with that key as encryption.local.key", col.table, strings.Join(keys, "/"))
	}
	sealed, err := s.keyring.SealForWrite(s.keyContext(ctx), field, value)
	if err != nil {
		return fmt.Errorf("%s %s: %w", col.table, strings.Join(keys, "/"), err)
	}
	if bytes.Equal(sealed, value) {
		return nil
	}
	return tx.Table(col.table).Where(cond, args...).UpdateColumn(col.column, sealed).Error
}
