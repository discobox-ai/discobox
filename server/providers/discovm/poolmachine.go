package discovm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"time"

	"github.com/discobox-ai/vm/pkg/engine"
	"github.com/discobox-ai/vm/pkg/guest"

	"github.com/discobox-ai/discobox/server/internal/model"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
)

const (
	// poolImage is the tag of the image a pool machine is created from in
	// the engine's store: the pool agent's twin (twins).
	poolImage = "discobox/pool-agent"
	// poolStartTimeout bounds a pool machine's boot until its disco-vm agent
	// answers.
	poolStartTimeout = 10 * time.Minute
	// poolStopTimeout is how long a pool machine gets to power off in order
	// before it is forced off.
	poolStopTimeout = time.Minute
)

// poolMachine hosts a pool in a disco-vm machine of its own, named by its pool
// ID so a create that was lost is found rather than duplicated. It is a remote
// driver's shape (ADR 26-10-09-106 §1): the machine runs the Linux pool image,
// which installs the pool agent.
//
// An operator reaches the host through disco-vm's own guest agent, never
// through the pool agent: the console and the log are what is asked for when
// the pool agent is what refuses to come up.
type poolMachine struct {
	engine *engine.Engine
	// shell is the console's command in the machine.
	shell []string
	// logs is the command that prints the machine's log, and logSource names
	// what it prints for the operator reading it.
	logs      func(sandbox.PoolLogOptions) []string
	logSource string
}

func newPoolMachine(e *engine.Engine) *poolMachine {
	return &poolMachine{
		engine:    e,
		shell:     []string{"/bin/bash", "-l"},
		logs:      journalCommand,
		logSource: "pool machine journal",
	}
}

// journalCommand reads a Linux pool machine's journal for this boot: the pool
// agent's unit and everything under it, which is what an operator needs when
// the agent will not register.
func journalCommand(opts sandbox.PoolLogOptions) []string {
	args := []string{"journalctl", "--no-pager", "--boot"}
	if opts.Tail > 0 {
		args = append(args, "--lines", strconv.Itoa(opts.Tail))
	} else {
		args = append(args, "--no-tail")
	}
	if opts.Follow {
		args = append(args, "--follow")
	}
	return args
}

// poolRuntimeState is what a pool row records of its machine.
type poolRuntimeState struct {
	Instance string `json:"instance"`
}

func poolMachineName(poolID string) string { return "discobox-pool-" + poolID }

// errNoPoolMachine is a pool whose machine was never created, or was removed.
var errNoPoolMachine = errors.New("the pool has no machine")

// instance finds the pool's machine, or reports errNoPoolMachine.
func (h *poolMachine) instance(pool *model.Pool) (*engine.Instance, error) {
	inst, err := h.engine.Get(poolMachineName(pool.ID))
	if errors.Is(err, engine.ErrNotFound) {
		return nil, fmt.Errorf("pool %s: %w: it is created when the pool reconciles", pool.ID, errNoPoolMachine)
	}
	return inst, err
}

func (h *poolMachine) ensurePoolHost(ctx context.Context, pool *model.Pool, begin func(context.Context) error) error {
	inst, err := h.instance(pool)
	if err != nil && !errors.Is(err, errNoPoolMachine) {
		return err
	}
	if inst != nil {
		state, err := h.state(ctx, pool, inst)
		if err != nil {
			return err
		}
		if state == engine.Running {
			return recordPoolMachine(pool, inst, false)
		}
	}
	if begin != nil {
		if err := begin(ctx); err != nil {
			return err
		}
	}
	if inst == nil {
		if inst, err = h.engine.Create(ctx, poolImage, engine.CreateOptions{Name: poolMachineName(pool.ID)}); err != nil {
			return fmt.Errorf("create pool %s's machine from %s: %w", pool.ID, poolImage, err)
		}
	}
	if err := h.engine.Start(ctx, inst, engine.StartOptions{Timeout: poolStartTimeout}); err != nil {
		return fmt.Errorf("start pool %s's machine: %w", pool.ID, err)
	}
	return recordPoolMachine(pool, inst, true)
}

func (h *poolMachine) repairPoolHost(ctx context.Context, pool *model.Pool, begin func(context.Context) error) error {
	inst, err := h.instance(pool)
	if errors.Is(err, errNoPoolMachine) {
		return h.ensurePoolHost(ctx, pool, begin)
	}
	if err != nil {
		return err
	}
	if _, err := h.state(ctx, pool, inst); err != nil {
		return err
	}
	if begin != nil {
		if err := begin(ctx); err != nil {
			return err
		}
	}
	// Forced off is still off, and off is what a restart needs.
	if err := h.engine.Stop(ctx, inst, poolStopTimeout); err != nil && h.engine.State(ctx, inst) != engine.Stopped {
		return fmt.Errorf("stop pool %s's machine: %w", pool.ID, err)
	}
	if err := h.engine.Start(ctx, inst, engine.StartOptions{Timeout: poolStartTimeout}); err != nil {
		return fmt.Errorf("start pool %s's machine: %w", pool.ID, err)
	}
	return recordPoolMachine(pool, inst, true)
}

// state is the machine's power state, as far as anyone can tell. A remote
// machine whose service does not answer (boxd's API) is neither: it may be
// running, so it is not started again, and it is not stopped by repair, which
// would end a healthy pool for a network fault. The pool reconciler waits on
// ErrPoolNotReachable rather than failing the pool.
func (h *poolMachine) state(ctx context.Context, pool *model.Pool, inst *engine.Instance) (engine.State, error) {
	state := h.engine.State(ctx, inst)
	if state == engine.Unknown {
		return state, fmt.Errorf("pool %s's machine cannot be asked for its state: %w", pool.ID, sandbox.ErrPoolNotReachable)
	}
	return state, nil
}

func (h *poolMachine) removePoolHost(ctx context.Context, pool *model.Pool) error {
	inst, err := h.instance(pool)
	if errors.Is(err, errNoPoolMachine) {
		return nil
	}
	if err != nil {
		return err
	}
	return h.engine.Remove(ctx, inst, true)
}

// recordPoolMachine stores the machine on the pool row. A machine that was just
// started has an agent that has not registered yet.
func recordPoolMachine(pool *model.Pool, inst *engine.Instance, started bool) error {
	state, err := json.Marshal(poolRuntimeState{Instance: inst.ID})
	if err != nil {
		return err
	}
	pool.RuntimeState = state
	if started {
		pool.Ready = false
		pool.Schedulable = false
		pool.Degraded = false
		pool.SetState(model.PoolStateRegistering)
	}
	return nil
}

// running finds the pool's machine and requires it to be running: the console
// and the log are read from inside it.
func (h *poolMachine) running(ctx context.Context, pool *model.Pool) (*engine.Instance, error) {
	inst, err := h.instance(pool)
	if err != nil {
		return nil, err
	}
	state, err := h.state(ctx, pool, inst)
	if err != nil {
		return nil, err
	}
	if state != engine.Running {
		return nil, fmt.Errorf("pool %s's machine is %s", pool.ID, state)
	}
	return inst, nil
}

func (h *poolMachine) openConsole(ctx context.Context, pool *model.Pool, opts sandbox.ConsoleOptions) (sandbox.PTY, error) {
	inst, err := h.running(ctx, pool)
	if err != nil {
		return nil, err
	}
	client := h.engine.Guest(inst)
	process, err := client.Exec(ctx, guest.ExecRequest{
		Argv: h.shell,
		// The guest agent adds nothing to its own environment, and a service's
		// has no TERM: without one, clear, less, top and vim refuse or degrade.
		Env:  []string{"TERM=xterm-256color"},
		TTY:  true,
		Rows: terminalSize(opts.Rows),
		Cols: terminalSize(opts.Cols),
	})
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("open a shell in pool %s's machine: %w", pool.ID, err)
	}
	return newGuestProcess(process, client.Close), nil
}

func (h *poolMachine) openLogs(ctx context.Context, pool *model.Pool, opts sandbox.PoolLogOptions) (*sandbox.PoolLogStream, error) {
	inst, err := h.running(ctx, pool)
	if err != nil {
		return nil, err
	}
	client := h.engine.Guest(inst)
	process, err := client.Exec(ctx, guest.ExecRequest{Argv: h.logs(opts)})
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("read pool %s's machine log: %w", pool.ID, err)
	}
	// The command reads nothing; its stdin is closed so it cannot wait on it.
	if err := process.CloseStdin(); err != nil {
		_ = process.Close()
		client.Close()
		return nil, err
	}
	return &sandbox.PoolLogStream{Source: h.logSource, ReadCloser: newGuestProcess(process, client.Close)}, nil
}

func terminalSize(n int) uint16 {
	if n <= 0 || n > 0xffff {
		return 0
	}
	return uint16(n)
}

// guestProcess is a process in a machine, read as one stream: a TTY carries
// stderr in its output anyway, and a log command explains itself on stderr,
// which is part of what the operator asked to read. Closing it kills the
// process, which is the only way a followed log ends.
type guestProcess struct {
	process *guest.Process
	release func()
	reader  *io.PipeReader

	done     chan struct{}
	code     int
	err      error
	closeOne sync.Once
}

func newGuestProcess(process *guest.Process, release func()) *guestProcess {
	reader, writer := io.Pipe()
	p := &guestProcess{process: process, release: release, reader: reader, done: make(chan struct{})}
	go func() {
		defer close(p.done)
		p.code, p.err = process.Wait(writer, writer)
		// A process that ran and exited ends the stream; its exit code is
		// Wait's to report, not the reader's. A stream that broke first is
		// the reader's error, so a followed log cut off by a dropped
		// connection does not read as one that ended.
		_ = writer.CloseWithError(p.err)
	}()
	return p
}

func (p *guestProcess) Read(b []byte) (int, error)  { return p.reader.Read(b) }
func (p *guestProcess) Write(b []byte) (int, error) { return p.process.Write(b) }

func (p *guestProcess) Resize(_ context.Context, rows, cols int) error {
	return p.process.Resize(terminalSize(rows), terminalSize(cols))
}

func (p *guestProcess) Wait(ctx context.Context) (int, error) {
	select {
	case <-p.done:
		return p.code, p.err
	case <-ctx.Done():
		return -1, ctx.Err()
	}
}

func (p *guestProcess) Close() error {
	p.closeOne.Do(func() {
		_ = p.process.Close()
		_ = p.reader.Close()
		<-p.done
		p.release()
	})
	return nil
}
