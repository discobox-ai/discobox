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
	// Kind identifies the problem to a program, where the problem is one a
	// caller has to act on differently. Most errors leave it empty.
	Kind Kind
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

// Kind is the stable identifier of a problem, served as the response's `type`
// so that a program can tell one refusal from another without reading the
// sentence a person reads. Most refusals have none: a kind exists only where a
// caller must do something different because of it, and every one of those is
// a decision somebody made on purpose.
//
// They are URNs rather than URLs. A problem type is an identifier and is not
// required to resolve to anything, and a URL here would be a promise to host a
// page at it forever.
type Kind string

// KindJudgingDisabled is a server that does not judge credential use answering
// a pool that asked it to. It is not a judge that failed, and a pool reads it
// as "stop asking for a while" rather than as a verdict — which is why it is
// worth telling apart from every other way an ask can come back with no
// answer.
const KindJudgingDisabled Kind = "urn:discobox:problem:judging-disabled"

// KindOf returns the problem kind err carries, if it carries one.
func KindOf(err error) (Kind, bool) {
	var carrier interface{ ProblemKind() Kind }
	if errors.As(err, &carrier) {
		kind := carrier.ProblemKind()
		return kind, kind != ""
	}
	return "", false
}

// ProblemKind makes a StatusError a carrier of its kind.
func (e StatusError) ProblemKind() Kind { return e.Kind }

// NewStatusErrorOfKind returns a status error a caller can recognize by its
// kind rather than by the sentence it carries.
func NewStatusErrorOfKind(status int, kind Kind, message string) error {
	return StatusError{Status: status, Kind: kind, Message: message}
}
