package endpoint

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/health"
)

const (
	defaultProbePath     = health.Path
	defaultProbeInterval = 100 * time.Millisecond
)

// LaunchOptions describes a local server process that can be started on demand.
type LaunchOptions struct {
	Endpoint      string
	LockPath      string
	LogPath       string
	Command       string
	Args          []string
	Env           []string
	ProbePath     string
	ProbeTimeout  time.Duration
	StartTimeout  time.Duration
	ReadyTimeout  time.Duration
	ProbeInterval time.Duration

	// OnProgress is called with each status a starting server reports, so a
	// caller can show what it is waiting for. Called only when the status
	// changes, and never once the server is ready.
	OnProgress func(health.Status)
}

// EnsureRunning starts the configured command when the local endpoint is not
// accepting requests. It serializes startup with a filesystem lock so concurrent
// CLIs do not spawn duplicate local servers.
//
// It reports whether this call is what started the server. A caller that
// launched one has left a process running on the user's machine that outlives
// it, which is worth saying out loud; one that found a server already up has
// nothing to report.
func EnsureRunning(ctx context.Context, opts LaunchOptions) (bool, error) {
	if opts.Endpoint == "" {
		opts.Endpoint = DefaultEndpoint()
	}
	// A server that is already up needs nothing, and one that is still starting
	// needs waiting on rather than a second process started alongside it.
	if status, err := probeEndpoint(ctx, opts); err == nil && !status.Starting() {
		return false, nil
	} else if err == nil {
		return false, waitReady(ctx, opts, time.Now().Add(opts.readyTimeout()))
	} else if !isProbeConnectionError(err) {
		return false, err
	}
	unlock, err := acquireLaunchLock(opts.lockPath())
	if err != nil {
		return false, err
	}
	defer unlock()
	if status, err := probeEndpoint(ctx, opts); err == nil && !status.Starting() {
		return false, nil
	} else if err == nil {
		return false, waitReady(ctx, opts, time.Now().Add(opts.readyTimeout()))
	} else if !isProbeConnectionError(err) {
		return false, err
	}
	if err := startDetached(ctx, opts); err != nil {
		return false, err
	}
	// Two deadlines, because two different things go wrong. A process that
	// never answers at all has died — misconfigured, or asked to run a command
	// it does not have — and there is no point waiting minutes for it. One that
	// answers "starting" is working, and cutting it off at ten seconds is how a
	// first run against an empty database looked like a failure.
	deadline := time.Now().Add(opts.startTimeout())
	var lastErr error
	for time.Now().Before(deadline) {
		status, err := probeEndpoint(ctx, opts)
		if err == nil {
			if !status.Starting() {
				return true, nil
			}
			return true, waitReady(ctx, opts, time.Now().Add(opts.readyTimeout()))
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return true, ctx.Err()
		case <-time.After(opts.probeInterval()):
		}
	}
	return true, fmt.Errorf("local server at %s never answered: %w%s", opts.Endpoint, lastErr, opts.logTail())
}

// waitReady polls a server that has answered "starting" until it is ready.
func waitReady(ctx context.Context, opts LaunchOptions, deadline time.Time) error {
	var last health.Status
	for time.Now().Before(deadline) {
		status, err := probeEndpoint(ctx, opts)
		switch {
		case err == nil && !status.Starting():
			return nil
		case err == nil:
			// On the step changing, not on every poll: uptime moves with each
			// answer, so comparing whole statuses reports the same step over
			// and over.
			if status.Status != last.Status || status.Phase != last.Phase {
				opts.progress(status)
				last = status
			}
		case !isProbeConnectionError(err):
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(opts.probeInterval()):
		}
	}
	phase := last.Phase
	if phase == "" {
		phase = "unknown"
	}
	return fmt.Errorf("local server at %s did not finish starting (last step: %s)%s", opts.Endpoint, phase, opts.logTail())
}

// probeEndpoint asks the endpoint how it is. A reachable server answers with a
// status; anything else is an error, and only a connection error means "not
// running" (see isProbeConnectionError).
func probeEndpoint(ctx context.Context, opts LaunchOptions) (health.Status, error) {
	baseURL, client, err := HTTPClient(opts.Endpoint, nil)
	if err != nil {
		return health.Status{}, err
	}
	probeCtx, cancel := context.WithTimeout(ctx, opts.probeTimeout())
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, baseURL+opts.probePath(), nil)
	if err != nil {
		return health.Status{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return health.Status{}, err
	}
	defer resp.Body.Close()
	var status health.Status
	// A body is not required: an older server, or one behind a proxy that
	// answers the probe itself, still counts as up.
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&status); err != nil {
		status = health.Status{}
	}
	switch {
	case resp.StatusCode == http.StatusServiceUnavailable && status.Starting():
		return status, nil
	case resp.StatusCode < 200 || resp.StatusCode >= 500:
		return health.Status{}, fmt.Errorf("local server probe returned %s", resp.Status)
	}
	if status.Status == "" {
		status.Status = health.StatusReady
	}
	return status, nil
}

func startDetached(ctx context.Context, opts LaunchOptions) error {
	if opts.Command == "" {
		return fmt.Errorf("server command is required")
	}
	// The child's output went nowhere, so a server that died on startup died
	// silently and the only symptom was a caller waiting out its timeout on a
	// socket nothing had bound. It goes to a file instead — opened here, before
	// either way of starting the process, because the systemd unit is told to
	// append to the same file and needs the directory to exist.
	logFile, err := openServerLog(opts.logPath(), opts.Command, opts.Args)
	if err != nil {
		return err
	}
	defer logFile.Close()
	if started, err := startUserService(ctx, opts); err != nil {
		return err
	} else if started {
		return nil
	}
	//nolint:gosec // The command is supplied by trusted CLI configuration for local server startup.
	cmd := exec.CommandContext(ctx, opts.Command, opts.Args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
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

// logPath is where a launched server's output is kept. See ServerLogPath.
func (o LaunchOptions) logPath() string {
	if o.LogPath != "" {
		return o.LogPath
	}
	return ServerLogPath()
}

// logTail is the last launch's output, for an error to carry, along with where
// the rest of it is. Empty when there is nothing to show, so it can be appended
// unconditionally.
func (o LaunchOptions) logTail() string {
	tail := lastServerLogLaunch(o.logPath())
	if tail == "" {
		return ""
	}
	return fmt.Sprintf("\n%s said (full log: %s):\n%s", o.Command, o.logPath(), tail)
}

// progress reports a starting server's status, when a caller asked to see it.
func (o LaunchOptions) progress(status health.Status) {
	if o.OnProgress != nil {
		o.OnProgress(status)
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

// startTimeout bounds how long to wait for a launched server to answer at all.
// Short, because a process that has not answered by now is not coming.
func (o LaunchOptions) startTimeout() time.Duration {
	if o.StartTimeout > 0 {
		return o.StartTimeout
	}
	return 10 * time.Second
}

// readyTimeout bounds how long to wait for a server that is answering
// "starting" to finish. Generous, because it is doing real work — migrating a
// database, reaching a registry — and reporting what it is doing while it does.
func (o LaunchOptions) readyTimeout() time.Duration {
	if o.ReadyTimeout > 0 {
		return o.ReadyTimeout
	}
	return 5 * time.Minute
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
