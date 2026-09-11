package sandboxruntime

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// EnvSandboxIdleTimeout carries the pool's sandbox idle timeout — how long a
// sandbox runs with nothing happening in it before it powers itself off
// (ADR 0108) — from its provider instance's pool policy into the pool agent,
// which writes it into a sandbox's sandbox.json at create and again before
// every start. Unset leaves each sandbox-agent on its own default.
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

// applySandboxIdleTimeout writes the pool's current idle timeout into a
// sandbox's sandbox.json, ahead of a start (ADR 0108 §3).
//
// The rest of the document is rendered once, at create, and the sandbox-agent
// reads it on every boot. Without this a sandbox would keep the timeout it was
// created with for its whole life, and a changed provider setting would reach
// only sandboxes created after it.
func (r *DockerSandboxRuntime) applySandboxIdleTimeout(sandboxID string) error {
	return applyIdleTimeout(filepath.Join(r.sandboxConfigRoot(sandboxID), sandboxDocumentName), r.sandboxIdleTimeout)
}

// applyIdleTimeout sets agentRuntime.idleTimeout in the sandbox.json at path,
// in the effective config and in its runtime provenance alike, and writes
// nothing when it already holds that value. A document that does not exist
// has nothing to update: the sandbox it belongs to cannot boot anyway, and
// failing its start over a stop policy would only hide why.
func applyIdleTimeout(path string, idleTimeout time.Duration) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var file sandboxDocumentFile
	if err := json.Unmarshal(data, &file); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	want := ""
	if idleTimeout > 0 {
		want = idleTimeout.String()
	}
	if file.AgentRuntime.IdleTimeout == want && file.Provenance.Runtime.AgentRuntime.IdleTimeout == want {
		return nil
	}
	file.AgentRuntime.IdleTimeout = want
	file.Provenance.Runtime.AgentRuntime.IdleTimeout = want
	out, err := json.MarshalIndent(&file, "", "  ")
	if err != nil {
		return err
	}
	return writeSandboxManifest(path, out)
}
