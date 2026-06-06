package jobqueue

import "time"

// Type identifies a class of jobs.
//
// Application packages should declare their own constants, for example:
//
//	const TypeSandboxProvision jobqueue.Type = "sandbox.provision"
type Type string

// Resource identifies the logical thing a job operates on.
//
// The dispatcher uses Resource to avoid running conflicting jobs concurrently.
// By default, the queue also uses Resource as the active-job uniqueness key:
// only one pending or running job may exist for a resource, regardless of job
// type.
type Resource struct {
	// Type is the application-defined resource kind, such as "sandbox",
	// "project", or "repository".
	Type string

	// ID is the stable identifier of the specific resource.
	ID string
}

// Payload is implemented by application-owned job payload structs.
//
// The queue serializes the concrete payload to JSON and stores it on Job.Payload.
// Executors are responsible for unmarshaling that JSON back into their concrete
// payload type.
type Payload interface {
	// JobType returns the executor type required to process this payload.
	JobType() Type

	// Resource returns the logical resource this job acts on. Jobs with the same
	// non-empty resource are serialized by the dispatcher.
	Resource() Resource
}

// Prioritized is an optional payload interface for overriding queue priority.
//
// Higher values run before lower values.
type Prioritized interface {
	// Priority returns the job's claim priority. Higher values run first.
	Priority() int
}

// MaxAttempter is an optional payload interface for overriding retry attempts.
type MaxAttempter interface {
	// MaxAttempts returns the maximum number of execution attempts before the job
	// becomes permanently failed.
	MaxAttempts() int
}

// Schedulable is an optional payload interface for delayed execution.
type Schedulable interface {
	// ScheduledAt returns the earliest time this job may be claimed.
	ScheduledAt() time.Time
}

// DuplicateAllower is an optional payload interface for allowing multiple
// active jobs against the same resource.
//
// This opt-out applies to the specific payload being enqueued. Duplicate jobs
// are still resource-serialized at execution time.
type DuplicateAllower interface {
	// AllowDuplicates returns true when Enqueue should permit another active job
	// for the same resource, even if the existing job has a different type.
	AllowDuplicates() bool
}
