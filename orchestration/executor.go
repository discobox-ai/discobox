package orchestration

import "context"

// Executor handles one application-defined job type.
//
// Executors usually live in application packages because they need concrete
// payload structs and domain services.
type Executor interface {
	// Type returns the job type this executor can process.
	Type() Type

	// Execute performs one job attempt. The dispatcher supplies a context bounded
	// by DispatcherConfig.JobTimeout. Returning an error fails the attempt and may
	// cause a retry depending on the job's MaxAttempts.
	Execute(ctx context.Context, job *Job) error
}

// GenerationAssertor may be implemented by executors whose jobs target a
// resource generation. The dispatcher invokes it after claiming the job and
// before Execute. Returning ErrJobCanceled, usually through Superseded, marks
// the job canceled without calling Execute.
type GenerationAssertor interface {
	AssertGeneration(ctx context.Context, job *Job) error
}

// ExecutorOption configures dispatcher behavior for one registered executor.
type ExecutorOption func(*executorConfig)

type executorConfig struct {
	concurrency int
}

// WithConcurrency sets the maximum number of jobs of this type that can run at
// the same time on one dispatcher. Values less than one should be treated as one
// by Register.
func WithConcurrency(limit int) ExecutorOption {
	return func(cfg *executorConfig) {
		cfg.concurrency = limit
	}
}
