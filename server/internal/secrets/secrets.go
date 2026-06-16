// Package secrets provides authenticated encryption for data stored at rest.
package secrets

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"strings"
)

const (
	keySize = 32
	prefix  = "discobox:v1:"
)

// Sealer encrypts and decrypts data at rest.
//
// Purpose names the field or secret class being encrypted. ResourceID binds
// the ciphertext to the owning resource. Implementations use both as
// additional authenticated data so ciphertext cannot be moved between fields
// or resources by copying database bytes.
type Sealer interface {
	Seal(ctx context.Context, purpose string, resourceID string, plaintext []byte) ([]byte, error)
	Open(ctx context.Context, purpose string, resourceID string, ciphertext []byte) ([]byte, error)
}

// AESGCMSealer implements Sealer with AES-256-GCM.
type AESGCMSealer struct {
	gcm cipher.AEAD
}

// NewAESGCMSealer creates an AES-256-GCM sealer from a 32-byte key.
func NewAESGCMSealer(key []byte) (*AESGCMSealer, error) {
	if len(key) != keySize {
		return nil, fmt.Errorf("encryption key must be %d bytes", keySize)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &AESGCMSealer{gcm: gcm}, nil
}

// NewAESGCMSealerFromBase64Key creates a sealer from a base64-encoded 32-byte
// key. The decoded key contains secret key material and must be protected like
// any other application secret.
func NewAESGCMSealerFromBase64Key(value string) (*AESGCMSealer, error) {
	key, err := DecodeBase64Key(value)
	if err != nil {
		return nil, err
	}
	return NewAESGCMSealer(key)
}

// DecodeBase64Key decodes and validates a base64-encoded 32-byte encryption
// key.
func DecodeBase64Key(value string) ([]byte, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, fmt.Errorf("encryption key is required")
	}
	key, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return nil, fmt.Errorf("decode encryption key: %w", err)
	}
	if len(key) != keySize {
		return nil, fmt.Errorf("encryption key must decode to %d bytes", keySize)
	}
	return key, nil
}

// GenerateBase64Key returns a new base64-encoded 32-byte key suitable for
// DISCOBOX_ENCRYPTION_KEY.
func GenerateBase64Key() (string, error) {
	key := make([]byte, keySize)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(key), nil
}

// IsSealed reports whether value uses this package's encrypted-at-rest format.
func IsSealed(value []byte) bool {
	return bytes.HasPrefix(value, []byte(prefix))
}

// Open decrypts ciphertext with sealer and returns a copy of plaintext. A nil
// sealer treats value as plaintext for tests and unencrypted deployments.
func Open(ctx context.Context, sealer Sealer, purpose string, resourceID string, value []byte) ([]byte, error) {
	if sealer == nil || len(value) == 0 {
		return append([]byte(nil), value...), nil
	}
	return sealer.Open(ctx, purpose, resourceID, value)
}

// SealIfUnsealed encrypts plaintext values and leaves already-sealed values
// unchanged after authenticating them with the supplied purpose and resource ID.
func SealIfUnsealed(ctx context.Context, sealer Sealer, purpose string, resourceID string, value []byte) ([]byte, error) {
	if sealer == nil || len(value) == 0 {
		return append([]byte(nil), value...), nil
	}
	if IsSealed(value) {
		if _, err := sealer.Open(ctx, purpose, resourceID, value); err != nil {
			return nil, fmt.Errorf("verify sealed value: %w", err)
		}
		return append([]byte(nil), value...), nil
	}
	return sealer.Seal(ctx, purpose, resourceID, value)
}

func (s *AESGCMSealer) Seal(_ context.Context, purpose string, resourceID string, plaintext []byte) ([]byte, error) {
	if s == nil {
		return append([]byte(nil), plaintext...), nil
	}
	if len(plaintext) == 0 {
		return nil, nil
	}
	nonce := make([]byte, s.gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	ciphertext := s.gcm.Seal(nil, nonce, plaintext, associatedData(purpose, resourceID))
	out := make([]byte, 0, len(prefix)+len(nonce)+len(ciphertext))
	out = append(out, prefix...)
	out = append(out, nonce...)
	out = append(out, ciphertext...)
	return out, nil
}

func (s *AESGCMSealer) Open(_ context.Context, purpose string, resourceID string, ciphertext []byte) ([]byte, error) {
	if s == nil {
		return append([]byte(nil), ciphertext...), nil
	}
	if len(ciphertext) == 0 {
		return nil, nil
	}
	if !bytes.HasPrefix(ciphertext, []byte(prefix)) {
		return nil, fmt.Errorf("unsupported ciphertext format")
	}
	body := ciphertext[len(prefix):]
	nonceSize := s.gcm.NonceSize()
	if len(body) < nonceSize {
		return nil, fmt.Errorf("ciphertext too short")
	}
	nonce := body[:nonceSize]
	encrypted := body[nonceSize:]
	return s.gcm.Open(nil, nonce, encrypted, associatedData(purpose, resourceID))
}

func associatedData(purpose, resourceID string) []byte {
	return []byte(purpose + "\x00" + resourceID)
}
