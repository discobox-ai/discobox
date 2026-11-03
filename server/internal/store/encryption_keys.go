package store

import (
	"context"
	"errors"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/discobox-ai/envelope/pkg/envelope"

	"github.com/discobox-ai/discobox/server/internal/model"
)

// EncryptionKeys stores the keyring's wrapped data keys in encryption_keys
// (ADR 26-11-02-902 §2).
//
// A value is often sealed inside a store transaction, and SQLite has one
// write connection, so a key read or written outside that transaction would
// wait on it forever. A transaction-bound Store hands its transaction to the
// keyring in the context it seals with (keyContext), and these methods use
// it. A scope's first key is made outside any transaction (Keyring.Ensure),
// so one never vanishes with a rollback.
type EncryptionKeys struct {
	write *gorm.DB
	read  *gorm.DB
}

var _ envelope.KeyStore = (*EncryptionKeys)(nil)

// NewEncryptionKeys returns the key store over the server's database. read
// must see every committed write (database.DB.CurrentRead), never a lagging
// replica: which keys a scope has decides what is written next (a scope with
// none gets one, a key on another provider is rewrapped), and a replica
// behind would mint a second key or report one missing (store/REVIEW.md).
// It must not be the write handle on SQLite either: a key read outside a
// transaction would then queue on the one write connection, behind a
// transaction that waits on the same key's fetch.
func NewEncryptionKeys(write, read *gorm.DB) *EncryptionKeys {
	return &EncryptionKeys{write: write, read: read}
}

type keyTxKey struct{}

// keyContext is ctx carrying s's transaction, if s is bound to one, for the
// keyring's reads and writes of data keys.
func (s *Store) keyContext(ctx context.Context) context.Context {
	if s.txWrite == nil {
		return ctx
	}
	return context.WithValue(ctx, keyTxKey{}, s.txWrite)
}

func (e *EncryptionKeys) db(ctx context.Context, write bool) (*gorm.DB, error) {
	if tx, ok := ctx.Value(keyTxKey{}).(*gorm.DB); ok && tx != nil {
		return tx.WithContext(ctx), nil
	}
	if e == nil || e.write == nil {
		return nil, errors.New("encryption key store has no database")
	}
	if write {
		return e.write.WithContext(ctx), nil
	}
	return e.read.WithContext(ctx), nil
}

func (e *EncryptionKeys) List(ctx context.Context, scope string) ([]envelope.Key, error) {
	db, err := e.db(ctx, false)
	if err != nil {
		return nil, err
	}
	var rows []model.EncryptionKey
	if err := db.Where("scope = ?", scope).Order("created_at ASC, id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	keys := make([]envelope.Key, 0, len(rows))
	for _, row := range rows {
		keys = append(keys, envelope.Key{
			Scope:         row.Scope,
			ID:            row.ID,
			Provider:      row.Provider,
			ProviderKeyID: row.ProviderKeyID,
			Wrapped:       row.Wrapped,
			CreatedAt:     row.CreatedAt,
		})
	}
	return keys, nil
}

// Put stores key, replacing the row of the same scope and ID: a rewrap
// stores the same data key wrapped again.
func (e *EncryptionKeys) Put(ctx context.Context, key envelope.Key) error {
	db, err := e.db(ctx, true)
	if err != nil {
		return err
	}
	row := model.EncryptionKey{
		Scope:         key.Scope,
		ID:            key.ID,
		Provider:      key.Provider,
		ProviderKeyID: key.ProviderKeyID,
		Wrapped:       key.Wrapped,
		CreatedAt:     key.CreatedAt,
	}
	return db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "scope"}, {Name: "id"}},
		DoUpdates: clause.AssignmentColumns([]string{"provider", "provider_key_id", "wrapped"}),
	}).Create(&row).Error
}

func (e *EncryptionKeys) Delete(ctx context.Context, scope string) error {
	db, err := e.db(ctx, true)
	if err != nil {
		return err
	}
	return db.Where("scope = ?", scope).Delete(&model.EncryptionKey{}).Error
}
