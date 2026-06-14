package orchestration

import (
	"crypto/rand"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
)

func NewID() (string, error) {
	id, err := ulid.New(ulid.Timestamp(time.Now()), rand.Reader)
	if err != nil {
		return "", err
	}
	return strings.ToLower(id.String()), nil
}

func NewIDString() string {
	id, err := NewID()
	if err != nil {
		panic(err)
	}
	return id
}
