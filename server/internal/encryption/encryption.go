// Package encryption seals what the server stores encrypted: each project's
// values with that project's own data key, wrapped by the configured
// provider (ADR 26-11-02-902).
package encryption

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/discobox-ai/envelope/pkg/envelope"
)

// ServerScope is the scope of what belongs to the server rather than to a
// project. Project IDs carry the proj_ prefix, so no project has it.
const ServerScope = "server"

// LegacyPrefix starts a value an earlier release sealed with encryptionKey.
// Open still reads one, and startup re-seals every one it finds.
const LegacyPrefix = "discobox:v1:"

// ErrLegacyKeyMissing is returned for a value sealed by an earlier release
// when no local key is configured to open it.
var ErrLegacyKeyMissing = errors.New("the value was sealed by an earlier release with encryptionKey, and encryption.local.key does not hold that key")

// Field is where a sealed value lives: the scope whose data key seals it,
// and the purpose and resource ID it is bound to, so its bytes cannot be
// moved to another row, column, or project and still open.
type Field struct {
	Scope      string
	Purpose    string
	ResourceID string
}

// The fields the server seals (ADR 26-11-02-902 §1). Each resource ID is the
// one the field was bound to before data keys, so a re-sealed value is bound
// as it was.

func SecretValue(projectID, secretID string) Field {
	return Field{Scope: projectID, Purpose: "secrets.value", ResourceID: projectID + "/" + secretID}
}

func SandboxSecretState(projectID, sandboxID string) Field {
	return Field{Scope: projectID, Purpose: "sandboxes.secret_state", ResourceID: projectID + "/" + sandboxID}
}

func SandboxInventory(projectID, sandboxID string) Field {
	return Field{Scope: projectID, Purpose: "sandbox_inventories.document", ResourceID: projectID + "/" + sandboxID}
}

// SandboxProviderSecrets is a provider instance's encrypted_config: the
// fields its definition types secret (ADR 26-10-31-694 §1).
func SandboxProviderSecrets(projectID, providerID string) Field {
	return Field{Scope: projectID, Purpose: "sandbox_provider_instances.encrypted_config", ResourceID: projectID + "/" + providerID}
}

func SandboxAccessIssuerKey(projectID, userID string) Field {
	return Field{Scope: projectID, Purpose: "sandbox_access_issuer_keys.private_key", ResourceID: projectID + "/" + userID}
}

// PoolAgentIssuerKey is the field of the key the server signs its requests
// to pool agents with, kept under stateKey in server_state.
func PoolAgentIssuerKey(stateKey string) Field {
	return Field{Scope: ServerScope, Purpose: "worker_agent_request_issuer.private_key", ResourceID: stateKey}
}

func (f Field) aad() []byte { return []byte(f.Purpose + "/" + f.ResourceID) }

func (f Field) legacyAAD() []byte { return []byte(f.Purpose + "\x00" + f.ResourceID) }

// Keyring seals and opens the server's stored values. A nil Keyring is a
// server with no provider, which stores values unsealed.
type Keyring struct {
	envelope *envelope.Keyring
	primary  envelope.Provider
	// legacy opens values an earlier release sealed with encryptionKey:
	// the local key and every retired one.
	legacy []cipher.AEAD
	stop   func()
}

// Sealed reports whether value is sealed in the current format.
func Sealed(value []byte) bool { return envelope.IsSealed(value) }

// Legacy reports whether value was sealed by an earlier release.
func Legacy(value []byte) bool { return bytes.HasPrefix(value, []byte(LegacyPrefix)) }

// Seal seals plaintext for f. An empty plaintext stays empty.
func (k *Keyring) Seal(ctx context.Context, f Field, plaintext []byte) ([]byte, error) {
	if k == nil || len(plaintext) == 0 {
		return clone(plaintext), nil
	}
	return k.envelope.Seal(ctx, f.Scope, f.aad(), plaintext)
}

// Open returns value's plaintext. It reads every form a stored value has:
// sealed now, sealed by an earlier release, and never sealed.
func (k *Keyring) Open(ctx context.Context, f Field, value []byte) ([]byte, error) {
	switch {
	case len(value) == 0:
		return clone(value), nil
	case Sealed(value):
		if k == nil {
			return nil, errors.New("the value is sealed, and no encryption.provider is configured to open it")
		}
		return k.envelope.Open(ctx, f.Scope, f.aad(), value)
	case Legacy(value):
		return k.openLegacy(f, value)
	default:
		return clone(value), nil
	}
}

// SealForWrite is what a writer stores for value, which may be plaintext or
// a value read back from the store. A sealed one is kept once it opens for
// f, so bytes copied from another row are refused rather than stored; one
// an earlier release sealed is sealed again in the current format.
func (k *Keyring) SealForWrite(ctx context.Context, f Field, value []byte) ([]byte, error) {
	if k == nil || len(value) == 0 {
		return clone(value), nil
	}
	if Sealed(value) {
		if _, err := k.Open(ctx, f, value); err != nil {
			return nil, fmt.Errorf("verify sealed value: %w", err)
		}
		return clone(value), nil
	}
	plaintext, err := k.Open(ctx, f, value)
	if err != nil {
		return nil, err
	}
	return k.Seal(ctx, f, plaintext)
}

// Ensure gives scope its data key if it has none. A scope whose first value
// can be sealed inside a database transaction is ensured outside one first:
// a key stored by a transaction that rolls back would stay cached, sealing
// values nothing could open later.
func (k *Keyring) Ensure(ctx context.Context, scope string) error {
	if k == nil {
		return nil
	}
	return k.envelope.Ensure(ctx, scope)
}

// Destroy deletes scope's data keys, after which nothing sealed for it opens.
func (k *Keyring) Destroy(ctx context.Context, scope string) error {
	if k == nil {
		return nil
	}
	return k.envelope.Destroy(ctx, scope)
}

// Rewrap wraps scope's data keys with the current provider and wrapping key,
// and reports how many it rewrapped.
func (k *Keyring) Rewrap(ctx context.Context, scope string) (int, error) {
	if k == nil {
		return 0, nil
	}
	return k.envelope.Rewrap(ctx, scope)
}

// Provider is the provider that wraps new data keys, and its wrapping key.
func (k *Keyring) Provider() (name, keyID string) {
	if k == nil {
		return "", ""
	}
	return k.primary.Name(), k.primary.KeyID()
}

// Close stops what keeps the provider's credentials current.
func (k *Keyring) Close() {
	if k != nil && k.stop != nil {
		k.stop()
	}
}

func (k *Keyring) openLegacy(f Field, value []byte) ([]byte, error) {
	if k == nil || len(k.legacy) == 0 {
		return nil, ErrLegacyKeyMissing
	}
	body := value[len(LegacyPrefix):]
	for _, aead := range k.legacy {
		if len(body) < aead.NonceSize() {
			return nil, errors.New("the value sealed by an earlier release is too short")
		}
		if plaintext, err := aead.Open(nil, body[:aead.NonceSize()], body[aead.NonceSize():], f.legacyAAD()); err == nil {
			return plaintext, nil
		}
	}
	return nil, fmt.Errorf("%w, or the value was altered", ErrLegacyKeyMissing)
}

// GenerateLocalKey returns a new local key, base64 as encryption.local.key
// takes it.
func GenerateLocalKey() (string, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(key), nil
}

func decodeLocalKey(setting, value string) ([]byte, error) {
	value, err := resolveFile(setting, value)
	if err != nil {
		return nil, err
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value))
	if err != nil {
		return nil, fmt.Errorf("%s: decode: %w", setting, err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("%s must decode to 32 bytes, not %d", setting, len(key))
	}
	return key, nil
}

func newLegacyAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func clone(b []byte) []byte { return append([]byte(nil), b...) }
