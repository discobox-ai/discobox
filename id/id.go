// Package id contains ID generation helpers.
package id

import (
	"crypto/rand"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
)

const (
	// GeneratedLength is the length of Discobox-generated lowercase ULID strings.
	GeneratedLength = 26
	// DefaultShortLength is the default display length for short Discobox IDs.
	DefaultShortLength = 8
)

func New() (string, error) {
	id, err := ulid.New(ulid.Timestamp(time.Now()), rand.Reader)
	if err != nil {
		return "", err
	}
	return strings.ToLower(id.String()), nil
}

func NewString() string {
	id, err := New()
	if err != nil {
		panic(err)
	}
	return id
}

// Short returns the conventional short display form for a Discobox ID.
func Short(id string) string {
	return ShortN(id, DefaultShortLength)
}

// ShortN returns the rightmost n characters of id, or id itself when shorter.
func ShortN(id string, n int) string {
	id = strings.TrimSpace(id)
	if IsFriendly(id) {
		return id
	}
	if n <= 0 || len(id) <= n {
		return id
	}
	return id[len(id)-n:]
}

// IsFriendly reports whether value is a typed, human-readable ID rather than a
// generated Discobox ID.
func IsFriendly(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && len(value) < GeneratedLength && strings.Contains(value, "_")
}

// IsShort reports whether value is shorter than a generated Discobox ID.
func IsShort(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && len(value) < GeneratedLength
}
