// Package apperrors defines shared sentinel and HTTP status errors.
package apperrors

import "errors"

var (
	// ErrNotFound indicates a requested resource does not exist.
	ErrNotFound = errors.New("not found")

	// ErrGenerationConflict indicates an observed resource generation is stale.
	ErrGenerationConflict = errors.New("generation conflict")
)

// StatusError carries an HTTP response status for API-facing errors.
type StatusError struct {
	Status  int
	Message string
}

func (e StatusError) Error() string {
	return e.Message
}

func (e StatusError) StatusCode() int {
	return e.Status
}

// NewStatusError returns an error carrying an HTTP status code.
func NewStatusError(status int, message string) error {
	return StatusError{Status: status, Message: message}
}
