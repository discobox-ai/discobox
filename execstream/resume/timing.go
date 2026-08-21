package resume

import (
	"fmt"
	"time"

	"github.com/discobox-ai/discobox/execstream/frame"
)

// TimingSource identifies which part of an attach produced a round-trip
// measurement.
type TimingSource string

const (
	// TimingHeartbeat is a physical transport probe. For the CLI websocket
	// transport it measures an end-to-end websocket ping/pong through the
	// control-plane proxies to the sandbox agent, without entering the exec shim.
	TimingHeartbeat TimingSource = "heartbeat"
	// TimingActionAcknowledgement measures from accepting a positioned process
	// action until the exec host acknowledges applying it. For frame.Input this
	// is the user-input delivery RTT through the shim and PTY boundary.
	TimingActionAcknowledgement TimingSource = "action_acknowledgement"
)

const (
	// DefaultHeartbeatInterval is the sampling cadence when timing is enabled.
	DefaultHeartbeatInterval = 2 * time.Second
	// DefaultHeartbeatTimeout bounds one physical transport probe.
	DefaultHeartbeatTimeout = 2 * time.Second
	// DefaultSlowAfter is the default completed-RTT slowdown threshold.
	DefaultSlowAfter = 250 * time.Millisecond
)

// TimingEvent is one latency sample from a resumable attach.
//
// RoundTrip is measured with the client's monotonic clock. Slow is a convenient
// classification using TimingOptions.SlowAfter; consumers should still retain
// RoundTrip so a UI can show the actual value or apply its own policy. Err is
// set when a heartbeat did not complete before its timeout.
type TimingEvent struct {
	At           time.Time
	Source       TimingSource
	RoundTrip    time.Duration
	Slow         bool
	Err          error
	Position     uint64
	ActionType   byte
	PayloadBytes int
	PendingBytes int
}

// Input reports whether this sample acknowledges terminal input rather than a
// signal or close-input action.
func (e TimingEvent) Input() bool {
	return e.Source == TimingActionAcknowledgement && e.ActionType == frame.Input
}

// TimingOptions enables latency observations for a resumable attach. See
// DESIGN.md in this package for source interpretation and consumer policy.
//
// Observe is synchronous on the stream's read or reconnect goroutine for
// action acknowledgements and on a dedicated heartbeat goroutine for probes.
// It must return promptly and must not call back into the observed Conn.
type TimingOptions struct {
	Observe func(TimingEvent)
	// HeartbeatInterval controls physical transport sampling. Zero uses
	// DefaultHeartbeatInterval.
	HeartbeatInterval time.Duration
	// HeartbeatTimeout bounds one physical transport probe. Zero uses
	// DefaultHeartbeatTimeout.
	HeartbeatTimeout time.Duration
	// SlowAfter marks completed samples as slow. Zero uses DefaultSlowAfter.
	SlowAfter time.Duration
}

type timingConfig struct {
	observe           func(TimingEvent)
	heartbeatInterval time.Duration
	heartbeatTimeout  time.Duration
	slowAfter         time.Duration
}

func resolveTimingOptions(opts TimingOptions) (timingConfig, error) {
	if opts.Observe == nil {
		return timingConfig{}, nil
	}
	config := timingConfig{
		observe:           opts.Observe,
		heartbeatInterval: opts.HeartbeatInterval,
		heartbeatTimeout:  opts.HeartbeatTimeout,
		slowAfter:         opts.SlowAfter,
	}
	if config.heartbeatInterval == 0 {
		config.heartbeatInterval = DefaultHeartbeatInterval
	}
	if config.heartbeatTimeout == 0 {
		config.heartbeatTimeout = DefaultHeartbeatTimeout
	}
	if config.slowAfter == 0 {
		config.slowAfter = DefaultSlowAfter
	}
	if config.heartbeatInterval < 0 {
		return timingConfig{}, fmt.Errorf("exec stream timing heartbeat interval must be non-negative, got %s", config.heartbeatInterval)
	}
	if config.heartbeatTimeout < 0 {
		return timingConfig{}, fmt.Errorf("exec stream timing heartbeat timeout must be non-negative, got %s", config.heartbeatTimeout)
	}
	if config.slowAfter < 0 {
		return timingConfig{}, fmt.Errorf("exec stream timing slow threshold must be non-negative, got %s", config.slowAfter)
	}
	return config, nil
}
