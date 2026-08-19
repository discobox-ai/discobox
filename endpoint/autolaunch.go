package endpoint

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	defaultProbePath     = "/healthz"
	defaultProbeInterval = 100 * time.Millisecond
)

// LaunchOptions describes a local server process that can be started on demand.
type LaunchOptions struct {
	Endpoint      string
	LockPath      string
	Command       string
	Args          []string
	Env           []string
	ProbePath     string
	ProbeTimeout  time.Duration
	StartTimeout  time.Duration
	ProbeInterval time.Duration
}

// EnsureRunning starts the configured command when the local endpoint is not
// accepting requests. It serializes startup with a filesystem lock so concurrent
// CLIs do not spawn duplicate local servers.
func EnsureRunning(ctx context.Context, opts LaunchOptions) error {
	if opts.Endpoint == "" {
		opts.Endpoint = DefaultEndpoint()
	}
	if err := probeEndpoint(ctx, opts); err == nil {
		return nil
	} else if !isProbeConnectionError(err) {
		return err
	}
	unlock, err := acquireLaunchLock(opts.lockPath())
	if err != nil {
		return err
	}
	defer unlock()
	if err := probeEndpoint(ctx, opts); err == nil {
		return nil
	} else if !isProbeConnectionError(err) {
		return err
	}
	if err := startDetached(ctx, opts); err != nil {
		return err
	}
	deadline := time.Now().Add(opts.startTimeout())
	var lastErr error
	for time.Now().Before(deadline) {
		err := probeEndpoint(ctx, opts)
		if err == nil {
			return nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(opts.probeInterval()):
		}
	}
	return fmt.Errorf("local server did not become ready at %s: %w", opts.Endpoint, lastErr)
}

func probeEndpoint(ctx context.Context, opts LaunchOptions) error {
	baseURL, client, err := HTTPClient(opts.Endpoint, nil)
	if err != nil {
		return err
	}
	probeCtx, cancel := context.WithTimeout(ctx, opts.probeTimeout())
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, baseURL+opts.probePath(), nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 500 {
		return fmt.Errorf("local server probe returned %s", resp.Status)
	}
	return nil
}

func startDetached(ctx context.Context, opts LaunchOptions) error {
	if opts.Command == "" {
		return fmt.Errorf("server command is required")
	}
	if started, err := startUserService(ctx, opts); err != nil {
		return err
	} else if started {
		return nil
	}
	//nolint:gosec // The command is supplied by trusted CLI configuration for local server startup.
	cmd := exec.CommandContext(ctx, opts.Command, opts.Args...)
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.Stdin = nil
	cmd.Env = append(os.Environ(), opts.Env...)
	setDetachedProcess(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start local server: %w", err)
	}
	return nil
}

func userServiceUnitName(opts LaunchOptions) string {
	sum := sha256.Sum256([]byte(opts.Endpoint))
	suffix := hex.EncodeToString(sum[:])[:16]
	return systemdUnitName(filepath.Base(opts.Command), suffix)
}

func systemdUnitName(name, suffix string) string {
	name = strings.TrimSuffix(name, filepath.Ext(name))
	name = strings.ToLower(name)
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	base := strings.Trim(b.String(), "-_")
	if base == "" {
		base = "discobox"
	}
	if !strings.HasPrefix(base, "discobox") {
		base = "discobox-" + base
	}
	return base + "-server-" + suffix
}

func (o LaunchOptions) lockPath() string {
	if o.LockPath != "" {
		return o.LockPath
	}
	endpoint, err := Parse(o.Endpoint)
	if err != nil {
		return filepath.Join(os.TempDir(), "discobox-server-startup.lock")
	}
	switch endpoint.Scheme {
	case "unix":
		return endpoint.Value + ".lock"
	case "npipe":
		return filepath.Join(os.TempDir(), "discobox-server-startup.lock")
	default:
		return filepath.Join(os.TempDir(), "discobox-server-startup.lock")
	}
}

func (o LaunchOptions) probePath() string {
	if o.ProbePath != "" {
		return o.ProbePath
	}
	return defaultProbePath
}

func (o LaunchOptions) probeTimeout() time.Duration {
	if o.ProbeTimeout > 0 {
		return o.ProbeTimeout
	}
	return 500 * time.Millisecond
}

func (o LaunchOptions) startTimeout() time.Duration {
	if o.StartTimeout > 0 {
		return o.StartTimeout
	}
	return 10 * time.Second
}

func (o LaunchOptions) probeInterval() time.Duration {
	if o.ProbeInterval > 0 {
		return o.ProbeInterval
	}
	return defaultProbeInterval
}

func isProbeConnectionError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	return true
}
