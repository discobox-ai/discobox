package secrets

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"gorm.io/gorm"

	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// retiredExchangeType is the secret type that held fields the server traded
// for a short-lived token. It is no longer a type (ADR 26-11-12-957): a row
// still carrying it is one stored before the upgrade, waiting for
// RetireExchangeSecrets.
const retiredExchangeType = "exchange"

// RetireExchangeSecrets turns every stored exchange secret into a token secret
// sent nowhere (ADR 26-11-12-957 §2). It runs at startup, after the keyring is
// ready and before anything is served or resolves a secret, because the value
// is sealed and has to be opened and sealed again: the database migration runs
// before there is a keyring to do it with. Once no row carries the type it
// does nothing, so it runs on every start, and one interrupted part-way is
// finished by the next.
//
// Each secret is converted in a transaction of its own:
//
//   - its type becomes token;
//   - its value is the key a person stored: the one stored field's value, or
//     with more than one, the fields as a JSON object of name to value, so
//     nothing a person typed is lost. The exchanged token and its expiry go.
//     A value that cannot be opened or read is dropped for an empty token,
//     logged, rather than failing the start;
//   - its routes become none, so it is sent nowhere whatever its bindings say.
//     Under exchange the key went to one URL from the server; kept with its
//     routes, the key itself would go on every one of them from every bound
//     sandbox (§3). Its route constraints stay: they only narrow;
//   - allowRaw is cleared, deleting its raw bindings: one handed over the
//     token, and would now hand over the key;
//   - its value lifetime is the key's: no TTL, no expiry, the format re-read
//     from the value unless a person set it;
//   - its rejections go with the old value (Store.UpdateSecret retracts them
//     on a replaced value), and its holders are marked, so a running sandbox's
//     pool drops the routes and value it holds.
//
// Plain bindings, grants and delegations stay: they resolve nothing while the
// secret has no routes, and work again once a person gives it one.
func (s *Service) RetireExchangeSecrets(ctx context.Context) error {
	retired, err := s.store.SecretsOfType(ctx, retiredExchangeType)
	if err != nil {
		return fmt.Errorf("list exchange secrets: %w", err)
	}
	for i := range retired {
		if err := s.retireExchangeSecret(ctx, &retired[i]); err != nil {
			return fmt.Errorf("convert exchange secret %s in project %s: %w", retired[i].ID, retired[i].ProjectID, err)
		}
	}
	return nil
}

func (s *Service) retireExchangeSecret(ctx context.Context, read *model.Secret) error {
	value, unreadable := exchangeKey(ctx, s.store, read)
	converted := false
	var sec *model.Secret
	if err := s.store.Transaction(ctx, func(txStore *store.Store, txDB *gorm.DB) error {
		// Read again in the transaction, and converted from that read: a
		// second server may have converted it already, and what is written
		// back is the row as it stands now.
		current, err := txStore.GetSecret(ctx, read.ProjectID, read.ID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if current.Type != retiredExchangeType {
			return nil
		}
		rawRevoked := current.AllowRaw
		current.Type = model.SecretTypeToken
		current.Routes = nil
		current.AllowRaw = false
		current.TTL = 0
		current.ValueExpiresAt = nil
		if !bytes.Equal(current.EncryptedValue, read.EncryptedValue) {
			// Its value moved since it was opened: open this one instead.
			value, unreadable = exchangeKey(ctx, txStore, current)
		}
		current.EncryptedValue = value
		if err := txStore.UpdateSecret(ctx, current); err != nil {
			return err
		}
		if rawRevoked {
			if err := s.deleteRawBindingsTx(ctx, txStore, txDB, current); err != nil {
				return err
			}
		}
		converted, sec = true, current
		if s.holders != nil {
			return s.holders.MarkSecretHoldersDirtyTx(ctx, txStore, txDB, current.ProjectID, current.ID)
		}
		return nil
	}); err != nil {
		return err
	}
	if !converted {
		return nil
	}
	// An unreadable value does not fail the start: it is logged, and the row
	// is converted without it (see exchangeKey).
	switch {
	case unreadable != nil:
		slog.Error("an exchange secret's value could not be read, so it is now a token secret with no value and no routes; a person stores its key again",
			"project", sec.ProjectID, "secret", sec.ID, "name", sec.Name, "error", unreadable)
	default:
		slog.Warn("an exchange secret is now a token secret with no routes, so it is sent nowhere until a person gives it one",
			"project", sec.ProjectID, "secret", sec.ID, "name", sec.Name)
	}
	return nil
}

// exchangeKey is the value an exchange secret keeps as a token secret: what a
// person stored, and nothing the server made. With one stored field, the
// token is that field; with more, the fields as a JSON object of name to
// value. A value that cannot be opened or read is an empty token, and why is
// returned beside it: one credential nobody can read does not stop a server
// starting (ADR 26-11-12-957 rejects refusing the upgrade over one), and a
// row left on the retired type would fail every listing of its project.
func exchangeKey(ctx context.Context, st *store.Store, sec *model.Secret) ([]byte, error) {
	empty := []byte(`{}`)
	plaintext, err := st.OpenSecretPlaintext(ctx, sec)
	if err != nil {
		return empty, err
	}
	var stored struct {
		Exchange map[string]string `json:"exchange"`
	}
	if plaintext != nil {
		if err := json.Unmarshal(plaintext, &stored); err != nil {
			return empty, fmt.Errorf("read its value: %w", err)
		}
	}
	var token string
	switch len(stored.Exchange) {
	case 0:
	case 1:
		for _, field := range stored.Exchange {
			token = field
		}
	default:
		fields, err := json.Marshal(stored.Exchange)
		if err != nil {
			return empty, err
		}
		token = string(fields)
	}
	//nolint:gosec // Secret values are marshaled before store encryption.
	value, err := json.Marshal(model.SecretValue{Token: token})
	if err != nil {
		return empty, err
	}
	return value, nil
}
