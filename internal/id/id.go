// Package id contains ID generation helpers.
package id

import "github.com/google/uuid"

func New() (string, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return "", err
	}
	return id.String(), nil
}

func NewString() string {
	id, err := New()
	if err != nil {
		panic(err)
	}
	return id
}
