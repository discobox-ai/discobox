// Package id contains ID generation helpers.
package id

import (
	"crypto/rand"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
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
