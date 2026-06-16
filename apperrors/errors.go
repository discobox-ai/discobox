// Package apperrors defines shared sentinel errors used across modules.
package apperrors

import "errors"

var (
	// ErrNotFound indicates a requested resource does not exist.
	ErrNotFound = errors.New("not found")

	// ErrGenerationConflict indicates an observed resource generation is stale.
	ErrGenerationConflict = errors.New("generation conflict")
)
