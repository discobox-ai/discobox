package execs

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	sddbus "github.com/coreos/go-systemd/v22/dbus"
	godbus "github.com/godbus/dbus/v5"
)

// unitType is the suffix every exec unit carries. Unit names are stored and
// passed around without it — nextUnitGeneration parses a bare name, and a
// stored name with the suffix would make every relaunch collide on generation
// 2 — so it is appended at the D-Bus boundary and stripped off everything
// coming back.
const unitType = ".service"

// unitPattern matches every generation of every exec unit, for the one call
// that lists them all (ADR 0115 §3).
const unitPattern = "discobox-exec-*" + unitType

// connectTimeout is how long connection waits for a dial to produce a usable
// connection before giving up on it.
const connectTimeout = 10 * time.Second

// connectionCheckInterval is how often a subscription checks that its
// connection is still up. It is a liveness check on one socket, not a query
// about any exec.
const connectionCheckInterval = 30 * time.Second

// unitChangeBuffer is how many unit-change notifications may queue before
// systemd's dispatcher starts dropping them. Drops are reported on the error
// channel and answered with a sweep, so the buffer only has to absorb a burst
// — a sandbox's units all transitioning at once during shutdown.
const unitChangeBuffer = 256

// SystemdRunner drives the sandbox's systemd over a persistent D-Bus
// connection (ADR 0115 §4). It replaced a runner that forked `systemd-run` and
// `systemctl` per call, which cost a process per exec per poll.
//
// The connection is dialed on first use rather than at construction, so a
// Manager can be built — in a test, or before systemd is reachable — without
// requiring a bus.
type SystemdRunner struct {
	mu   sync.Mutex
	conn *sddbus.Conn
	dial func(context.Context) (*sddbus.Conn, error)
	// life bounds every connection this runner dials, and is canceled only by
	// Close. It is deliberately not any caller's context: godbus closes a
	// connection when the context it was dialed with is done (newConn spawns a
	// goroutine on conn.ctx.Done, and Connected reports that context's error),
	// so a connection dialed inside an HTTP handler would die when that handler
	// returned — taking the watcher's subscription down with it, because the
	// connection is shared.
	life   context.Context
	cancel context.CancelFunc
	// connCancel releases the context the current connection was dialed with.
	// Kept beside the connection rather than discarded on a successful dial,
	// so a run of reconnects cannot accumulate context children on life.
	connCancel context.CancelFunc
}

// NewSystemdRunner returns a runner that talks to the system bus.
//
// It must be the system bus rather than systemd's private socket at
// /run/systemd/private: the private socket needs no dbus-daemon and would
// serve every method call here, but Subscribe registers through AddMatch on
// the bus object, which only a dbus-daemon answers. Watch is required (ADR
// 0115 §2), so the connection that serves it is the one to have.
func NewSystemdRunner() *SystemdRunner {
	life, cancel := context.WithCancel(context.Background())
	return &SystemdRunner{
		life:   life,
		cancel: cancel,
		dial: func(ctx context.Context) (*sddbus.Conn, error) {
			return sddbus.NewSystemConnectionContext(ctx)
		},
	}
}

// connection returns a live connection, dialing one if this is the first call
// or if the last one was dropped.
//
// The caller's context is not passed to the dial: it bounds the *call*, while
// the connection outlives every call made on it. See SystemdRunner.life.
func (r *SystemdRunner) connection(_ context.Context) (*sddbus.Conn, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conn != nil && r.conn.Connected() {
		return r.conn, nil
	}
	r.dropConnection()
	// A zero-value runner still needs one lifetime that Close can end. Assigning
	// it here rather than substituting a local keeps Close's promise true: a
	// local would leave Close canceling nothing and the next call dialing again.
	if r.life == nil {
		r.life, r.cancel = context.WithCancel(context.Background())
	}
	if r.dial == nil {
		r.dial = func(ctx context.Context) (*sddbus.Conn, error) {
			return sddbus.NewSystemConnectionContext(ctx)
		}
	}
	if err := r.life.Err(); err != nil {
		return nil, fmt.Errorf("connect to systemd: %w", err)
	}
	// The context handed to the dial *owns the connection*: godbus stores it on
	// the Conn and closes the connection when it is done. So it cannot also be
	// the deadline — a context canceled when this function returns would close
	// the connection on the way out, handing every caller a conn already dead.
	//
	// It does still have to be cancelable, because the dial is not always
	// interruptible on its own: dbusAuthConnection calls Conn.Auth after the
	// transport is up, and Auth reads with no deadline, so a bus that accepts
	// and never finishes authenticating would block this goroutine forever.
	// Canceling closes the transport and unwedges it.
	//
	// Both properties together: cancel on the timeout path, never on success.
	// A successful dial's cancel is kept beside its connection and called when
	// that connection is dropped, so retries cannot pile up context children.
	dialCtx, cancelDial := context.WithCancel(r.life)
	type dialed struct {
		conn *sddbus.Conn
		err  error
	}
	// Buffered, so the dial goroutine always completes its send and never
	// blocks on a reader that has given up.
	results := make(chan dialed, 1)
	// Captured under the lock rather than read from r inside the goroutine:
	// the goroutine outlives this critical section, so reading the field there
	// would be a race the moment anything assigns it twice.
	dial := r.dial
	go func() {
		conn, err := dial(dialCtx)
		results <- dialed{conn: conn, err: err}
	}()
	timer := time.NewTimer(connectTimeout)
	defer timer.Stop()
	select {
	case result := <-results:
		if result.err != nil {
			cancelDial()
			return nil, fmt.Errorf("connect to systemd: %w", result.err)
		}
		r.conn, r.connCancel = result.conn, cancelDial
		return result.conn, nil
	case <-timer.C:
		cancelDial()
		go func() {
			if late := <-results; late.conn != nil {
				late.conn.Close()
			}
		}()
		return nil, fmt.Errorf("connect to systemd: %w", context.DeadlineExceeded)
	}
}

// dropConnection closes the current connection and releases the context that
// owned it. The caller holds r.mu.
func (r *SystemdRunner) dropConnection() {
	if r.conn != nil {
		r.conn.Close()
		r.conn = nil
	}
	if r.connCancel != nil {
		r.connCancel()
		r.connCancel = nil
	}
}

// Close ends the runner: the connection is dropped and no further one is
// dialed.
func (r *SystemdRunner) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cancel != nil {
		r.cancel()
	}
	r.dropConnection()
}

func (r *SystemdRunner) Start(ctx context.Context, req StartRequest) (StartResult, error) {
	if len(req.Command) == 0 || strings.TrimSpace(req.Command[0]) == "" {
		return StartResult{}, fmt.Errorf("exec command is required")
	}
	exe, err := os.Executable()
	if err != nil {
		return StartResult{}, err
	}
	argv, err := shimArgv(exe, req)
	if err != nil {
		return StartResult{}, err
	}
	conn, err := r.connection(ctx)
	if err != nil {
		return StartResult{}, err
	}
	name := unitFullName(req.Unit)
	job := make(chan string, 1)
	if _, err := conn.StartTransientUnitContext(ctx, name, "replace", unitProperties(req, argv), job); err != nil {
		return StartResult{}, fmt.Errorf("start %s: %w", name, err)
	}
	select {
	case result := <-job:
		// "done" is the only result that means the unit was started; the job
		// being canceled or having failed is a start failure, and reporting it
		// here is what records exec.start.failed with a reason.
		if result != "done" {
			return StartResult{}, fmt.Errorf("start %s: job %s", name, result)
		}
	case <-ctx.Done():
		return StartResult{}, ctx.Err()
	}
	return StartResult{Unit: req.Unit}, nil
}

// shimArgv is the command line the transient unit runs: this same binary, as
// the exec shim, carrying the exec's inputs as base64 JSON. The shim keeps the
// unit's own identity — no User=/Group= is set on the unit — because it drops
// to the run user itself after it has set up the PTY and the socket.
func shimArgv(exe string, req StartRequest) ([]string, error) {
	commandJSON, err := json.Marshal(req.Command)
	if err != nil {
		return nil, err
	}
	startupCommandJSON, err := json.Marshal(req.StartupCommand)
	if err != nil {
		return nil, err
	}
	envJSON, err := json.Marshal(req.Env)
	if err != nil {
		return nil, err
	}
	userJSON, err := json.Marshal(req.User)
	if err != nil {
		return nil, err
	}
	metadataJSON, err := json.Marshal(req.Metadata)
	if err != nil {
		return nil, err
	}
	argv := []string{
		exe,
		"exec-shim",
		"--exec-id", req.ID,
		"--unit", req.Unit,
		"--workdir", req.Workdir,
		"--socket", req.SocketPath,
		"--runtime", req.RuntimePath,
		"--database", req.DatabasePath,
		"--rows", strconv.Itoa(int(req.Rows)),
		"--cols", strconv.Itoa(int(req.Cols)),
		"--command", base64.StdEncoding.EncodeToString(commandJSON),
		"--startup-command", base64.StdEncoding.EncodeToString(startupCommandJSON),
		"--env", base64.StdEncoding.EncodeToString(envJSON),
		"--user", base64.StdEncoding.EncodeToString(userJSON),
		"--metadata", base64.StdEncoding.EncodeToString(metadataJSON),
	}
	if req.TTY {
		argv = append(argv, "--tty")
	}
	return argv, nil
}

// unitProperties are what `systemd-run --collect --property=...` used to spell
// on a command line. CollectMode is --collect: a transient unit that failed is
// unloaded rather than kept for a reset-failed, which is what lets every run
// take a fresh unit generation (ADR 0038 §2).
func unitProperties(req StartRequest, argv []string) []sddbus.Property {
	props := []sddbus.Property{
		sddbus.PropDescription("Discobox exec " + req.ID),
		sddbus.PropExecStart(argv, false),
		{Name: "KillMode", Value: godbus.MakeVariant("control-group")},
		{Name: "CollectMode", Value: godbus.MakeVariant("inactive-or-failed")},
	}
	if workdir := strings.TrimSpace(req.Workdir); workdir != "" {
		props = append(props, sddbus.Property{
			Name:  "WorkingDirectory",
			Value: godbus.MakeVariant(workdir),
		})
	}
	if env := unitEnvironment(req.Env); len(env) > 0 {
		props = append(props, sddbus.Property{
			Name:  "Environment",
			Value: godbus.MakeVariant(env),
		})
	}
	return props
}

// unitEnvironment renders the exec's environment as systemd's Environment
// property. Entries are sorted so a unit's properties do not depend on map
// iteration order.
func unitEnvironment(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	out := make([]string, 0, len(env))
	for key, value := range env {
		if strings.TrimSpace(key) != "" {
			out = append(out, key+"="+value)
		}
	}
	sort.Strings(out)
	return out
}

func (r *SystemdRunner) Stop(ctx context.Context, unit string) error {
	if strings.TrimSpace(unit) == "" {
		return nil
	}
	conn, err := r.connection(ctx)
	if err != nil {
		return err
	}
	name := unitFullName(unit)
	job := make(chan string, 1)
	if _, err := conn.StopUnitContext(ctx, name, "replace", job); err != nil {
		if isAlreadyStoppedError(err) {
			return nil
		}
		return fmt.Errorf("stop %s: %w", name, err)
	}
	select {
	case <-job:
		// Any job result ends the run as far as the caller is concerned: the
		// unit is not running once the job is off the queue, and a canceled
		// stop job means something else already stopped it.
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// isMissingUnitError reports whether systemd said, in as many words, that the
// unit does not exist. Only the typed D-Bus error counts: this answer decides
// that an exec is gone, and an exec declared gone is relaunched over the top of
// whatever is really running, so guessing from error text is not good enough.
func isMissingUnitError(err error) bool {
	if err == nil {
		return false
	}
	var dbusErr godbus.Error
	return errors.As(err, &dbusErr) && dbusErr.Name == "org.freedesktop.systemd1.NoSuchUnit"
}

// isAlreadyStoppedError is the laxer test Stop may use: the worst outcome of a
// false positive there is reporting a stop that was already a no-op.
func isAlreadyStoppedError(err error) bool {
	if err == nil {
		return false
	}
	if isMissingUnitError(err) {
		return true
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "not loaded") ||
		strings.Contains(text, "could not be found") ||
		strings.Contains(text, "not found")
}

func (r *SystemdRunner) Status(ctx context.Context, unit string) (UnitStatus, error) {
	if strings.TrimSpace(unit) == "" {
		return UnitStatus{}, fmt.Errorf("unit is required")
	}
	conn, err := r.connection(ctx)
	if err != nil {
		return UnitStatus{}, err
	}
	return unitStatus(ctx, conn, unitFullName(unit))
}

// unitStatus reads one unit's properties. GetAll answers for a unit systemd
// has never heard of the same way `systemctl show` did — a full property set
// reporting LoadState "not-found" — so a vanished unit is a status, not an
// error, and Loaded is what separates it from one that merely stopped.
func unitStatus(ctx context.Context, conn *sddbus.Conn, name string) (UnitStatus, error) {
	props, err := conn.GetAllPropertiesContext(ctx, name)
	if err != nil {
		if isMissingUnitError(err) {
			return UnitStatus{Unit: unitBaseName(name), Status: StatusLost}, nil
		}
		return UnitStatus{}, err
	}
	return unitStatusFromProperties(props), nil
}

func (r *SystemdRunner) List(ctx context.Context) ([]UnitStatus, error) {
	conn, err := r.connection(ctx)
	if err != nil {
		return nil, err
	}
	units, err := conn.ListUnitsByPatternsContext(ctx, nil, []string{unitPattern})
	if err != nil {
		return nil, err
	}
	out := make([]UnitStatus, 0, len(units))
	for _, unit := range units {
		// ListUnitsByPatterns carries only load/active/sub state; the exit
		// status, PID, and timestamps need the unit's own properties. These are
		// round trips on an open connection, not processes.
		status, err := unitStatus(ctx, conn, unit.Name)
		if err != nil {
			continue
		}
		out = append(out, status)
	}
	return out, nil
}

// Watch delivers the bare name of every exec unit whose properties changed,
// for as long as ctx lives (ADR 0115 §2). The returned channel is closed when
// the subscription ends, which is the caller's signal to fall back to sweeping.
//
// An error means no subscription was established at all — no dbus-daemon, or
// systemd refused it — and the caller runs degraded rather than blind.
func (r *SystemdRunner) Watch(ctx context.Context) (<-chan string, error) {
	conn, err := r.connection(ctx)
	if err != nil {
		return nil, err
	}
	if err := conn.Subscribe(); err != nil {
		return nil, fmt.Errorf("subscribe to systemd: %w", err)
	}
	updates := make(chan *sddbus.PropertiesUpdate, unitChangeBuffer)
	dropped := make(chan error, 8)
	conn.SetPropertiesSubscriber(updates, dropped)
	names := make(chan string, unitChangeBuffer)
	// A connection that drops takes the signals with it silently — systemd is
	// not going to tell us it stopped talking. Without this check the watcher
	// would wait forever on a channel nothing can write to; closing names is
	// what sends it back to sweeping.
	health := time.NewTicker(connectionCheckInterval)
	go func() {
		defer close(names)
		defer health.Stop()
		defer conn.SetPropertiesSubscriber(nil, nil)
		for {
			select {
			case <-ctx.Done():
				return
			case <-health.C:
				if !conn.Connected() {
					return
				}
			case update := <-updates:
				if update == nil || !isExecUnit(update.UnitName) {
					continue
				}
				select {
				case names <- unitBaseName(update.UnitName):
				case <-ctx.Done():
					return
				}
			case <-dropped:
				// systemd's dispatcher drops updates rather than blocking when
				// the buffer fills. The names are lost, so the only honest
				// answer is to make the reader sweep: an empty name says
				// "something changed, but not which".
				select {
				case names <- "":
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return names, nil
}

func isExecUnit(name string) bool {
	return strings.HasPrefix(name, "discobox-exec-")
}

// unitFullName is the unit name systemd answers to: names are stored bare, and
// a name that already carries a unit-type suffix is left alone.
func unitFullName(unit string) string {
	unit = strings.TrimSpace(unit)
	if unit == "" || strings.HasSuffix(unit, unitType) {
		return unit
	}
	return unit + unitType
}

// unitBaseName is the inverse: what gets stored on an Exec.
func unitBaseName(name string) string {
	return strings.TrimSuffix(strings.TrimSpace(name), unitType)
}

func unitStatusFromProperties(props map[string]any) UnitStatus {
	// systemd answers for a unit it has never heard of with a full property set
	// reporting it inactive, so LoadState is the only signal that separates
	// "this unit ran and is gone" from "this unit does not exist"; a transient
	// unit lost to a reboot reads not-found here.
	status := UnitStatus{
		Unit:   unitBaseName(propString(props, "Id")),
		Loaded: loadedState(propString(props, "LoadState")),
	}
	active := propString(props, "ActiveState")
	sub := propString(props, "SubState")
	switch active {
	case "active", "activating", "reloading":
		status.Active = true
		status.Status = StatusRunning
	case "failed":
		status.Status = StatusFailed
	case "inactive":
		status.Status = StatusExited
	default:
		status.Status = StatusLost
	}
	if sub == "dead" && status.Status == StatusRunning {
		status.Status = StatusExited
	}
	pid := propInt64(props, "MainPID")
	if pid == 0 {
		pid = propInt64(props, "ExecMainPID")
	}
	status.PID = pid
	if code := propInt64(props, "ExecMainStatus"); code != 0 || status.Status == StatusFailed {
		status.ExitCode = &code
	}
	if result := strings.TrimSpace(propString(props, "Result")); result != "" && result != "success" {
		status.Error = result
	}
	if started := propTime(props, "ActiveEnterTimestamp"); started != nil {
		status.StartedAt = started
	}
	if exited := propTime(props, "InactiveEnterTimestamp"); exited != nil {
		status.ExitedAt = exited
	}
	return status
}

// loadedState reports whether systemd still has a unit definition for the unit.
// An unreported LoadState counts as loaded: the property is missing only if the
// property set is unexpected, and treating that as a vanished unit would
// declare live execs lost.
func loadedState(value string) bool {
	switch strings.TrimSpace(value) {
	case "not-found", "error", "bad-setting":
		return false
	default:
		return true
	}
}

func propString(props map[string]any, name string) string {
	value, _ := props[name].(string)
	return value
}

// propInt64 reads a numeric property. systemd types these narrowly and
// differently — PIDs are uint32, an exit status is int32 — so every integer
// shape a property may arrive in is accepted rather than assuming one.
func propInt64(props map[string]any, name string) int64 {
	switch value := props[name].(type) {
	case int32:
		return int64(value)
	case int64:
		return value
	case uint32:
		return int64(value)
	case uint64:
		return int64(value)
	case int:
		return int64(value)
	default:
		return 0
	}
}

// propTime reads one of systemd's timestamps, which are microseconds since the
// epoch. Zero means the transition never happened.
func propTime(props map[string]any, name string) *time.Time {
	micros := propInt64(props, name)
	if micros <= 0 {
		return nil
	}
	at := time.UnixMicro(micros).UTC()
	return &at
}
