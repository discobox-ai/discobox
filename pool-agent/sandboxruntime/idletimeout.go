package sandboxruntime

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// EnvSandboxIdleTimeout carries the pool's sandbox idle timeout — how long a
// sandbox runs with nothing happening in it before it powers itself off
// (ADR 0108) — from its provider instance's pool policy into the pool agent,
// which delivers it in every sandbox's runtime-config document: a changed one
// reaches a running sandbox at the next status poll, and applies there at once
// (ADR 26-10-08-127 §5). Unset leaves each sandbox-agent on its own default.
const EnvSandboxIdleTimeout = "DISCOBOX_SANDBOX_IDLE_TIMEOUT"

// ConfiguredSandboxIdleTimeout resolves EnvSandboxIdleTimeout, returning zero
// when it is unset.
//
// An unparsable or non-positive value is an error rather than a silent
// fallback. The provider configuration it was rendered from has already been
// validated, so a bad value here is a bug to surface, not a setting to guess
// at — and either guess is wrong for someone: a sandbox that stops out from
// under its user, or one that never stops.
func ConfiguredSandboxIdleTimeout() (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(EnvSandboxIdleTimeout))
	if value == "" {
		return 0, nil
	}
	timeout, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", EnvSandboxIdleTimeout, err)
	}
	if timeout <= 0 {
		return 0, fmt.Errorf("%s must be greater than 0, got %s", EnvSandboxIdleTimeout, value)
	}
	return timeout, nil
}
