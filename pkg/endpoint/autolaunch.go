package endpoint

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptrace"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/discobox-ai/discobox/pkg/health"
	"golang.org/x/mod/semver"
)

const (
	// StartupErrorFileEnv carries a private, per-launch refusal record from the
	// server to its launcher, including when systemd owns the child.
	StartupErrorFileEnv  = "DISCOBOX_SERVER_START_ERROR_FILE"
	defaultProbePath     = health.Path
	defaultProbeInterval = 100 * time.Millisecond
	// probesBeforeGone is how many consecutive probes finding nothing listening
	// mean a server that had been answering has exited. See waitReady.
	probesBeforeGone = 3
)

// Command is a program to launch, and the arguments to launch it with.
type Command struct {
	Path string
	Args []string
}

// LaunchOptions describes a local server process that can be started on demand.
type LaunchOptions struct {
	Endpoint string
	LockPath string
	LogPath  string
	// Command resolves the program to start. It is called at most once, and
	// only when a server actually has to be started — never for one that is
	// already running.
	//
	// A func rather than a path because resolving it can cost something: the
	// CLI's server is a separate binary it may have to download and verify
	// first (ADR 0099), and a build with no server to download fails to
	// resolve one at all. Neither is a price a caller should pay to discover
	// that the server it wanted is already up.
	Command      func(context.Context) (Command, error)
	Env          []string
	ProbePath    string
	ProbeTimeout time.Duration
	StartTimeout time.Duration
	ReadyTimeout time.Duration
	// UpgradeTimeout bounds the wait for a server upgrading its pools, from
	// when that is first seen. The server bounds it itself
	// (health.PoolUpgradeWait), so the default outlasts that.
	UpgradeTimeout time.Duration
	ProbeInterval  time.Duration
	// ExpectedVersion is the version carried by the launching binary. When a
	// local server reports an older semantic version, EnsureRunning replaces it
	// before returning.
	ExpectedVersion string

	// OnProgress reports launch steps and each status a starting server reports,
	// so a caller can show what it is waiting for. Never called for a server
	// that is already ready and needs no upgrade.
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
	replacedOlder := false
	if opts.Endpoint == "" {
		opts.Endpoint = DefaultEndpoint()
	}
	// A server that is already up needs nothing, unless it is older, and one
	// that is still starting, or too busy to answer in time, needs waiting on
	// rather than a second process started alongside it: that one would ask it
	// to shut down (#1308).
	if status, found, err := probeSettled(ctx, opts); err != nil {
		return false, err
	} else if found && !olderServer(status, opts.ExpectedVersion) {
		return false, nil
	}
	opts.progress(health.Status{Status: health.StatusStarting, Phase: "waiting for the startup lock"})
	unlock, err := acquireLaunchLock(opts.lockPath())
	if err != nil {
		return false, err
	}
	defer unlock()
	replaceOlder := false
	// A server found here, waited on or not, is done with unless it is older:
	// launching after a wait would start a second server beside the one just
	// waited out.
	if status, found, err := probeSettled(ctx, opts); err != nil {
		return false, err
	} else if found {
		if !olderServer(status, opts.ExpectedVersion) {
			return false, nil
		}
		replaceOlder = true
	}
	// Resolved before an older server is stopped, never after. Resolving can
	// mean downloading a server and the images it runs (ADR 0099, ADR 0113) —
	// minutes, on the first run of a new version — and the server being
	// replaced goes on answering for all of it. Only the switch-over is a gap.
	//
	// Under the launch lock and after the last probe, so it is done once per
	// machine that actually needs a server rather than once per CLI invocation,
	// and two CLIs racing to start one do not both download it.
	command, err := resolveCommand(ctx, opts)
	if err != nil {
		return false, err
	}
	opts.Env = append(opts.Env, "DISCOBOX_SERVER_HANDOFF=")
	if replaceOlder {
		opts.progress(health.Status{Status: health.StatusStarting, Phase: "stopping the older server"})
		if err := replaceOlderServer(ctx, opts); err != nil {
			return false, err
		}
		replacedOlder = true
		opts.Env = append(opts.Env, "DISCOBOX_SERVER_HANDOFF="+opts.Endpoint)
	}
	// Create before either launch path, and keep the file private. An empty
	// record is not a failure. Each launch has its own file so an earlier
	// refusal cannot poison a subsequent attempt.
	if err := os.MkdirAll(filepath.Dir(opts.logPath()), 0o700); err != nil {
		return false, err
	}
	failure, err := os.CreateTemp(filepath.Dir(opts.logPath()), ".server-start-error-*")
	if err != nil {
		return false, err
	}
	failurePath := failure.Name()
	if err := failure.Close(); err != nil {
		_ = os.Remove(failurePath)
		return false, err
	}
	defer os.Remove(failurePath)
	opts.Env = append(opts.Env, StartupErrorFileEnv+"="+failurePath)
	opts.progress(health.Status{Status: health.StatusStarting, Phase: "launching the server"})
	child, err := startDetached(ctx, opts, command)
	if err != nil {
		return false, err
	}
	// Two deadlines, because two different things go wrong. A process that
	// never answers at all has died — misconfigured, or asked to run a command
	// it does not have — and there is no point waiting minutes for it. One that
	// answers "starting" is working, and cutting it off at ten seconds is how a
	// first run against an empty database looked like a failure.
	startWait := opts.startTimeout()
	if replacedOlder {
		// The old listener disappears before its server has finished draining
		// providers and released the data-directory singleton. The replacement
		// is alive but cannot bind during that interval, so this is a known
		// shutdown/start transition rather than a child that never came up.
		startWait = opts.readyTimeout()
	}
	deadline := time.Now().Add(startWait)
	var lastErr error
	for time.Now().Before(deadline) {
		status, err := probeEndpoint(ctx, opts)
		if err == nil {
			if !status.Waiting() {
				opts.progress(status)
				return true, nil
			}
			_, err := waitReady(ctx, opts, time.Now().Add(opts.readyTimeout()))
			return true, err
		}
		if errors.Is(err, errProbeUnanswered) {
			// Listening, and too busy to answer: it has come up, and what is
			// left is the wait for it to be ready.
			_, err := waitReady(ctx, opts, time.Now().Add(opts.readyTimeout()))
			return true, err
		}
		lastErr = err
		// Cancellation first, because it is also what killed the process. A
		// child started with exec.CommandContext is killed by os/exec the
		// moment ctx is done, so an interrupted CLI sees its own server exit
		// and would otherwise report it as a server that died on startup.
		if ctx.Err() != nil {
			return true, ctx.Err()
		}
		// A launched process that has already exited is not going to answer,
		// and the start timeout is the wrong thing to spend on it: a server
		// that refuses to start — no WSL Containers on Windows, no harness —
		// dies in milliseconds, and its explanation is in the log before the
		// first probe. Waiting the rest out says nothing and delays every
		// command that autolaunches after it.
		if data, err := os.ReadFile(failurePath); err == nil && len(data) > 0 {
			return true, fmt.Errorf("local server refused startup: %s", data)
		}
		if child.exited() {
			return true, fmt.Errorf("local server at %s exited during startup%s", opts.Endpoint, opts.logTail())
		}
		select {
		case <-ctx.Done():
			return true, ctx.Err()
		case <-time.After(opts.probeInterval()):
		}
	}
	if replacedOlder {
		return true, fmt.Errorf("local server at %s did not finish shutdown handoff: %w%s", opts.Endpoint, lastErr, opts.logTail())
	}
	return true, fmt.Errorf("local server at %s never answered: %w%s", opts.Endpoint, lastErr, opts.logTail())
}

// olderServer is deliberately one-way. An older CLI must not downgrade a
// newer server, and an unversioned development build or legacy health response
// does not provide enough evidence to replace a running process.
func olderServer(status health.Status, expected string) bool {
	return semver.IsValid(status.Version) && semver.IsValid(expected) && semver.Compare(status.Version, expected) < 0
}

func replaceOlderServer(ctx context.Context, opts LaunchOptions) error {
	target, err := Parse(opts.Endpoint)
	if err != nil {
		return err
	}
	baseURL, client, err := HTTPClient(target, nil)
	if err != nil {
		return err
	}
	readinessDeadline := time.Now().Add(opts.readyTimeout())
	for {
		shutdownCtx, cancel := context.WithTimeout(ctx, opts.probeTimeout())
		req, err := http.NewRequestWithContext(shutdownCtx, http.MethodPost, baseURL+"/shutdown", nil)
		if err != nil {
			cancel()
			return err
		}
		resp, err := client.Do(req)
		if err != nil {
			cancel()
			return fmt.Errorf("stop older local server: %w", err)
		}
		_ = resp.Body.Close()
		cancel()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			break
		}
		status, probeErr := probeEndpoint(ctx, opts)
		if resp.StatusCode == http.StatusServiceUnavailable && probeErr == nil && status.Waiting() {
			if _, err := waitReady(ctx, opts, readinessDeadline); err != nil {
				return err
			}
			continue
		}
		return fmt.Errorf("stop older local server: %s", resp.Status)
	}
	// Server shutdown includes draining reconcilers and VM backends. Give it the
	// same long allowance as startup after the process has begun answering,
	// rather than the short deadline used for a child that never binds.
	deadline := time.Now().Add(opts.readyTimeout())
	for time.Now().Before(deadline) {
		_, err := probeEndpoint(ctx, opts)
		if isProbeConnectionError(err) {
			return nil
		}
		// One too busy to answer has not let go yet.
		if err != nil && !errors.Is(err, errProbeUnanswered) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(opts.probeInterval()):
		}
	}
	return fmt.Errorf("older local server at %s did not stop", opts.Endpoint)
}

// probeSettled probes the endpoint and reports whether a server is there.
// When one is there but not ready to be judged — starting, upgrading its
// pools, or too busy to answer — it waits until the server is ready and
// returns that answer, which says its version. A server already known to be
// older is returned as it is: it is replaced, not waited on. Nothing there is
// found false with no error; an error is one to give up on, a wait that
// failed included.
func probeSettled(ctx context.Context, opts LaunchOptions) (health.Status, bool, error) {
	status, err := probeEndpoint(ctx, opts)
	switch {
	case errors.Is(err, errProbeUnanswered) || (err == nil && status.Waiting() && !olderServer(status, opts.ExpectedVersion)):
		status, err = waitReady(ctx, opts, time.Now().Add(opts.readyTimeout()))
		return status, err == nil, err
	case err == nil:
		return status, true, nil
	case isProbeConnectionError(err):
		return health.Status{}, false, nil
	default:
		return health.Status{}, false, err
	}
}

// waitReady polls a server that has answered "starting" or "upgrading pools",
// or is listening and too busy to answer, until it is ready, and returns the
// ready answer. Every caller enters on such a server, so it is known to be
// there from the first probe on.
//
// Upgrading pools gets a deadline of its own, from when it is first seen: the
// server bounds it (health.PoolUpgradeWait) and reports ready once that runs
// out, so the client's deadline is that and a margin, never the start's.
func waitReady(ctx context.Context, opts LaunchOptions, deadline time.Time) (health.Status, error) {
	// last is the step the server last reported, for an error to name;
	// drawn is what the progress line shows, which a silent server changes
	// without reporting a step.
	var last, drawn health.Status
	unanswered, upgrading, silent := 0, false, false
	for time.Now().Before(deadline) {
		status, err := probeEndpoint(ctx, opts)
		switch {
		case err == nil && !status.Waiting():
			opts.progress(status)
			return status, nil
		case err == nil:
			if status.UpgradingPools() && !upgrading {
				upgrading = true
				deadline = time.Now().Add(opts.upgradeTimeout())
			}
			unanswered, silent = 0, false
			// On the step changing, not on every poll: uptime moves with each
			// answer, so comparing whole statuses reports the same step over
			// and over.
			if status.Status != drawn.Status || status.Phase != drawn.Phase || status.PoolsUpgrading != drawn.PoolsUpgrading || !slices.Equal(status.Warnings, drawn.Warnings) {
				opts.progress(status)
				drawn = status
			}
			last = status
		case errors.Is(err, errProbeUnanswered):
			// Listening, and too busy to answer in time: still there. Said
			// once per silence, and recorded as drawn, so the server's next
			// answer redraws its step even when it is the step shown before.
			if !silent {
				drawn = health.Status{Status: health.StatusStarting, Phase: "waiting for the server to answer"}
				opts.progress(drawn)
			}
			unanswered, silent = 0, true
		case !isProbeConnectionError(err):
			return health.Status{}, err
		case listenerGone(err):
			// It was answering and now nothing listens. A server holds its
			// listeners for the whole of startup and releases them only when
			// the process ends, so this is one that gave up — a step that
			// failed — rather than one that is busy, and waiting out the
			// generous ready deadline for a process that has exited is minutes
			// of saying nothing when its last words are already in the log.
			unanswered++
			if unanswered >= probesBeforeGone {
				return health.Status{}, fmt.Errorf("local server at %s stopped while starting (last step: %s)%s",
					opts.Endpoint, lastStep(last), opts.logTail())
			}
		default:
			// Anything else is no proof that the listener is gone — above all
			// a TCP dial that ran out the probe timeout, which a loaded
			// machine produces as well as a closed port on Windows — so the
			// wait goes on, and the deadline bounds it (#1298: three timeouts
			// in a row read as "stopped" while the server went on upgrading
			// its pools for minutes). A timeout against a listener is the
			// case above.
			unanswered = 0
		}
		select {
		case <-ctx.Done():
			return health.Status{}, ctx.Err()
		case <-time.After(opts.probeInterval()):
		}
	}
	// Said by what the last probe found: a server whose listener has just gone
	// is not one to call running, and one that is listening but has never
	// answered was not starting.
	if unanswered > 0 {
		return health.Status{}, fmt.Errorf("local server at %s stopped answering before it finished starting (last step: %s)%s",
			opts.Endpoint, lastStep(last), opts.logTail())
	}
	if silent {
		return health.Status{}, fmt.Errorf("local server at %s is listening but has not answered its health check (last step: %s)%s",
			opts.Endpoint, lastStep(last), opts.logTail())
	}
	return health.Status{}, fmt.Errorf("local server at %s is still running but did not finish starting in time (last step: %s)%s",
		opts.Endpoint, lastStep(last), opts.logTail())
}

// lastStep names the last step a server reported, for an error that has to say
// what the wait was on. A server upgrading pools says so in its status and
// count, and may report no phase at all.
func lastStep(status health.Status) string {
	if phase := strings.TrimSpace(status.Phase); phase != "" {
		return phase
	}
	if status.UpgradingPools() {
		return fmt.Sprintf("upgrading pools (%d left)", status.PoolsUpgrading)
	}
	return "unknown"
}

// errProbeUnanswered is a probe that ran out its own timeout against a
// listener: a server is there, too busy to answer in time. See probeEndpoint.
var errProbeUnanswered = errors.New("local server did not answer in time")

// probeEndpoint asks the endpoint how it is. A reachable server answers with a
// status; anything else is an error. A server that is there but did not answer
// within the probe timeout is errProbeUnanswered; otherwise only a connection
// error means "not running" (see isProbeConnectionError).
func probeEndpoint(ctx context.Context, opts LaunchOptions) (health.Status, error) {
	target, err := Parse(opts.Endpoint)
	if err != nil {
		return health.Status{}, err
	}
	baseURL, client, err := HTTPClient(target, nil)
	if err != nil {
		return health.Status{}, err
	}
	probeCtx, cancel := context.WithTimeout(ctx, opts.probeTimeout())
	defer cancel()
	var connected atomic.Bool
	traceCtx := httptrace.WithClientTrace(probeCtx, &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) { connected.Store(true) },
	})
	req, err := http.NewRequestWithContext(traceCtx, http.MethodGet, baseURL+opts.probePath(), nil)
	if err != nil {
		return health.Status{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		// The probe's own timeout, against something listening, is a server
		// too busy to answer — on a loaded host, for seconds at a time — and
		// not an absent one (#1308). Something is listening when the
		// connection was accepted, or when a named pipe's dial ran out the
		// time: a missing pipe fails at once, so only one whose instances are
		// all busy makes the dial wait (go-winio retries ERROR_PIPE_BUSY until
		// the context ends). A unix socket's dial never waits; a full backlog
		// fails it at once (below). A TCP dial that
		// runs out the timeout is not a listener: Windows refuses a closed
		// loopback port only after about two seconds, and reading that as a
		// server would stall every cold start there.
		if ctx.Err() == nil && errors.Is(probeCtx.Err(), context.DeadlineExceeded) &&
			(connected.Load() || target.Scheme == "npipe") {
			return health.Status{}, fmt.Errorf("%w (%s): %w", errProbeUnanswered, opts.probeTimeout(), err)
		}
		// A dial that fails at once is no server, but for one failure: a
		// listener whose accept backlog is full, which is a server too busy
		// even to accept. See backlogFull.
		if target.Scheme == "unix" && backlogFull(err) {
			return health.Status{}, fmt.Errorf("%w (accept backlog full): %w", errProbeUnanswered, err)
		}
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
	case resp.StatusCode == http.StatusServiceUnavailable && status.Waiting():
		return status, nil
	case resp.StatusCode < 200 || resp.StatusCode >= 500:
		return health.Status{}, fmt.Errorf("local server probe returned %s", resp.Status)
	}
	if status.Status == "" {
		status.Status = health.StatusReady
	}
	return status, nil
}

// launchedProcess is the server process this call started, when it is one this
// process can watch. Its only question is whether that process is still alive,
// which is what tells a server that died on startup apart from one that is
// merely slow to bind.
//
// A server started as a user service has none: systemd owns the process, this
// one never had a handle on it, and the probe loop's own reasoning about a
// server that stopped answering is what covers it there.
type launchedProcess struct {
	done <-chan struct{}
}

// exited reports whether the launched process is already gone. A nil receiver —
// nothing this process is watching — is never gone, so the caller has no case
// to switch on.
func (p *launchedProcess) exited() bool {
	if p == nil {
		return false
	}
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// resolveCommand asks the caller which program to start.
func resolveCommand(ctx context.Context, opts LaunchOptions) (Command, error) {
	if opts.Command == nil {
		return Command{}, fmt.Errorf("server command is required")
	}
	command, err := opts.Command(ctx)
	if err != nil {
		return Command{}, err
	}
	if command.Path == "" {
		return Command{}, fmt.Errorf("server command is required")
	}
	return command, nil
}

// startDetached starts the resolved server program in the background.
func startDetached(ctx context.Context, opts LaunchOptions, command Command) (*launchedProcess, error) {
	// The child's output went nowhere, so a server that died on startup died
	// silently and the only symptom was a caller waiting out its timeout on a
	// socket nothing had bound. It goes to a file instead — opened here, before
	// either way of starting the process, because the systemd unit is told to
	// append to the same file and needs the directory to exist.
	logFile, err := openServerLog(opts.logPath(), command.Path, command.Args)
	if err != nil {
		return nil, err
	}
	defer logFile.Close()
	if started, err := startUserService(ctx, opts, command, logFile); err != nil {
		return nil, err
	} else if started {
		return nil, nil
	}
	//nolint:gosec // The command is supplied by trusted CLI configuration for local server startup.
	cmd := exec.CommandContext(ctx, command.Path, command.Args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil
	cmd.Env = append(os.Environ(), opts.Env...)
	setDetachedProcess(cmd)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start local server: %w", err)
	}
	// Reaped rather than left behind: the server is meant to outlive this
	// process, so this waits only to learn that it did not — and a Wait that
	// returns also releases the process handle and stops the context watchdog
	// os/exec left running for it.
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = cmd.Wait()
	}()
	return &launchedProcess{done: done}, nil
}

func userServiceUnitName(opts LaunchOptions, command Command) string {
	sum := sha256.Sum256([]byte(opts.Endpoint))
	suffix := hex.EncodeToString(sum[:])[:16]
	return systemdUnitName(filepath.Base(command.Path), suffix)
}

func systemdUnitName(name, suffix string) string {
	name = strings.TrimSuffix(name, filepath.Ext(name))
	name = strings.ToLower(name)
	// The program is discobox-server, and the unit is named after what it is
	// rather than after the file: "-server" is appended below, and a machine
	// upgraded from the CLI that used to run the server in-process must not
	// end up with a second unit under a second name for the same endpoint.
	name = strings.TrimSuffix(name, "-server")
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
	return fmt.Sprintf("\nthe local server said (full log: %s):\n%s", o.logPath(), tail)
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

// upgradeMargin is how much longer than the server's own bound a client waits
// on a server upgrading its pools: past the bound the server answers ready,
// and a client cut off at the same instant would fail on that answer's eve.
const upgradeMargin = time.Minute

func (o LaunchOptions) upgradeTimeout() time.Duration {
	if o.UpgradeTimeout > 0 {
		return o.UpgradeTimeout
	}
	return health.PoolUpgradeWait + upgradeMargin
}

func (o LaunchOptions) probeInterval() time.Duration {
	if o.ProbeInterval > 0 {
		return o.ProbeInterval
	}
	return defaultProbeInterval
}

// listenerGone reports whether a probe found nothing listening at all: the
// connection refused, or the socket or pipe no longer there. Unlike a probe
// that timed out, that is what a server that has exited looks like.
func listenerGone(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, errConnRefused)
}

func isProbeConnectionError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, errProbeUnanswered) {
		return false
	}
	return true
}
