package jobqueue

import "errors"

var (
	// ErrJobAlreadyExists is returned by low-level unique job creation when a
	// second active job would own the same resource, regardless of job type.
	ErrJobAlreadyExists = errors.New("job already exists for resource")

	// ErrExecutorAlreadyRegistered is returned when two executors are registered
	// for the same job type.
	ErrExecutorAlreadyRegistered = errors.New("executor already registered for job type")

	// ErrExecutorNotRegistered is used when a claimed job has no matching executor.
	ErrExecutorNotRegistered = errors.New("executor not registered for job type")

	// ErrJobNotFound is returned when a requested job row does not exist.
	ErrJobNotFound = errors.New("job not found")

	// ErrJobCanceled marks an execution attempt as intentionally canceled
	// instead of failed.
	ErrJobCanceled = errors.New("job canceled")
)

type canceledError struct {
	message string
}

func (e canceledError) Error() string {
	if e.message == "" {
		return ErrJobCanceled.Error()
	}
	return e.message
}

func (e canceledError) Unwrap() error {
	return ErrJobCanceled
}

func Canceled(message string) error {
	return canceledError{message: message}
}
