// Package apperrors defines shared sentinel and HTTP status errors.
package apperrors

import (
	"errors"
	"net/http"
)

var (
	// ErrNotFound indicates a requested resource does not exist.
	ErrNotFound = errors.New("not found")

	// ErrGenerationConflict indicates an observed resource generation is stale.
	ErrGenerationConflict = errors.New("generation conflict")
)

// StatusError carries an HTTP response status for API-facing errors.
//
// Cause is the condition the status was chosen for, kept so a caller can still
// match the sentinel with errors.Is while the API keeps the status and the
// message it serves. It is optional: a status error that has nothing more
// specific to say leaves it nil.
type StatusError struct {
	Status  int
	Message string
	Cause   error
}

func (e StatusError) Error() string {
	return e.Message
}

func (e StatusError) StatusCode() int {
	return e.Status
}

func (e StatusError) Unwrap() error {
	return e.Cause
}

// NewStatusError returns an error carrying an HTTP status code.
func NewStatusError(status int, message string) error {
	return StatusError{Status: status, Message: message}
}

// NotFound returns a 404 status error that still matches the sentinel err came
// with, so a caller past the API boundary can keep using errors.Is on it while
// the API serves the status and the message.
func NotFound(err error, message string) error {
	if errors.Is(err, ErrNotFound) {
		return StatusError{Status: http.StatusNotFound, Message: message, Cause: err}
	}
	return err
}
