package dockerworker

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

	"github.com/discobox-ai/discobox/server/internal/model"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
)

const (
	// consoleHostRoot is where the console sees the pool host's own filesystem.
	// The host is mounted rather than entered so the console keeps its own image
	// — the pool-agent image, which already carries the tools an operator wants
	// and is already on every daemon that hosts a pool.
	consoleHostRoot = "/host"
	// consoleConfigLayoutVersion changes whenever the console container's shape
	// changes, so an existing console is replaced instead of being reused with
	// the previous namespaces or mounts.
	consoleConfigLayoutVersion = 1
	consoleShell               = "/bin/bash"

	// LabelPoolConsole marks the pool host's console container. It is
	// deliberately not LabelPoolAgent: pool runtime drift detection reconciles
	// and removes containers carrying that label, and the console is not a pool
	// runtime.
	LabelPoolConsole = "discobox.pool_console"
	// LabelConsoleConfig records the console layout the container was created
	// from, compared on open to detect drift.
	LabelConsoleConfig = "discobox.pool_console.config_revision"
)

// OpenConsole attaches to the pool host's administrative console: a privileged
// root shell in the host's PID, IPC, network, UTS, and cgroup namespaces, with
// the host filesystem at /host and the host's Docker socket in place.
//
// The console container is created once per pool host and reattached
// afterwards, so a long-running capture or trace survives a detach, and every
// attach lands on the same shell. It runs on the daemon the pool's containers
// run on, reached through the driver's Docker client — which is what makes it
// useful for debugging a backend whose pool agent never came up.
func (e *Engine) OpenConsole(ctx context.Context, provider *model.SandboxProviderInstance, pool *model.Pool, opts sandbox.ConsoleOptions) (sandbox.PTY, error) {
	if pool == nil || strings.TrimSpace(pool.ID) == "" {
		return nil, errors.New("pool is required")
	}
	lease, err := e.driver.AcquireDockerClient(ctx, pool.ID)
	if err != nil {
		return nil, fmt.Errorf("reach the pool host's Docker daemon: %w", err)
	}
	session, err := e.attachConsole(ctx, lease, provider, pool, opts)
	if err != nil {
		lease.Release()
		return nil, err
	}
	return session, nil
}

func (e *Engine) attachConsole(ctx context.Context, lease *DockerClientLease, provider *model.SandboxProviderInstance, pool *model.Pool, opts sandbox.ConsoleOptions) (*consoleSession, error) {
	cli := lease.Client
	id, err := e.ensureConsoleContainer(ctx, cli, provider, pool)
	if err != nil {
		return nil, err
	}
	// Size the TTY before attaching so the shell's first repaint is already at
	// the caller's size rather than at the 80x24 the container was created with.
	resizeConsole(ctx, cli, id, opts.Rows, opts.Cols)
	attached, err := cli.ContainerAttach(ctx, id, client.ContainerAttachOptions{
		Stream: true,
		Stdin:  true,
		Stdout: true,
		Stderr: true,
	})
	if err != nil {
		return nil, fmt.Errorf("attach to pool console: %w", err)
	}
	return &consoleSession{client: cli, lease: lease, containerID: id, attached: attached}, nil
}

// ensureConsoleContainer returns the pool host's console container, creating it
// when absent, starting it when its shell exited, and replacing it when it was
// built from a different image or console layout.
func (e *Engine) ensureConsoleContainer(ctx context.Context, cli *client.Client, provider *model.SandboxProviderInstance, pool *model.Pool) (string, error) {
	name := ConsoleContainerName(pool.ID)
	existing, err := cli.ContainerInspect(ctx, name, client.ContainerInspectOptions{})
	switch {
	case err == nil:
		if e.shouldReplaceConsoleContainer(existing.Container) {
			if _, err := cli.ContainerRemove(ctx, existing.Container.ID, client.ContainerRemoveOptions{Force: true}); err != nil && !cerrdefs.IsNotFound(err) {
				return "", err
			}
			break
		}
		if existing.Container.State != nil && existing.Container.State.Running {
			return existing.Container.ID, nil
		}
		// The console's shell exits when an operator types exit; the next
		// console starts it again and gets a fresh shell.
		if _, err := cli.ContainerStart(ctx, existing.Container.ID, client.ContainerStartOptions{}); err != nil {
			return "", err
		}
		return existing.Container.ID, nil
	case !cerrdefs.IsNotFound(err):
		return "", err
	}
	return e.createConsoleContainer(ctx, cli, provider, pool, name)
}

func (e *Engine) createConsoleContainer(ctx context.Context, cli *client.Client, provider *model.SandboxProviderInstance, pool *model.Pool, name string) (string, error) {
	// The console runs the pool-agent image too, and an operator can open one
	// on a daemon that has never hosted a pool container.
	if err := e.ensureImage(ctx, cli); err != nil {
		return "", err
	}
	created, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config:     e.consoleConfig(provider, pool),
		HostConfig: e.consoleHostConfig(),
		Name:       name,
	})
	if err != nil {
		// Another console raced this one to the same name; whichever container
		// exists now is the console.
		if cerrdefs.IsConflict(err) {
			inspect, inspectErr := cli.ContainerInspect(ctx, name, client.ContainerInspectOptions{})
			if inspectErr != nil {
				return "", err
			}
			if inspect.Container.State == nil || !inspect.Container.State.Running {
				if _, startErr := cli.ContainerStart(ctx, inspect.Container.ID, client.ContainerStartOptions{}); startErr != nil {
					return "", startErr
				}
			}
			return inspect.Container.ID, nil
		}
		return "", fmt.Errorf("create pool console: %w", err)
	}
	if _, err := cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return "", fmt.Errorf("start pool console: %w", err)
	}
	return created.ID, nil
}

func (e *Engine) consoleConfig(provider *model.SandboxProviderInstance, pool *model.Pool) *container.Config {
	return &container.Config{
		Image:  e.cfg.Image,
		Labels: e.consoleLabels(provider, pool),
		// The pool-agent image's entrypoint is the agent binary and its
		// healthcheck probes the agent's port; a console runs neither.
		Entrypoint:  []string{},
		Cmd:         []string{consoleShell, "-l"},
		Healthcheck: &container.HealthConfig{Test: []string{"NONE"}},
		Tty:         true,
		OpenStdin:   true,
		AttachStdin: true,
		Env: []string{
			"TERM=xterm-256color",
			"DISCOBOX_POOL_ID=" + pool.ID,
			"DISCOBOX_PROJECT_ID=" + pool.ProjectID,
			"DISCOBOX_HOST_ROOT=" + consoleHostRoot,
		},
		WorkingDir: consoleHostRoot,
	}
}

func (e *Engine) consoleHostConfig() *container.HostConfig {
	socket := cleanAbsPath(e.cfg.DockerSocket)
	if socket == "" {
		socket = dockerSocketPath
	}
	return &container.HostConfig{
		Privileged: true,
		// Every namespace the host has: an operator debugging a backend needs to
		// see the host's processes, its network, and its mounts as the host sees
		// them, not a container's private view of each.
		PidMode:      container.PidMode("host"),
		IpcMode:      container.IPCModeHost,
		UTSMode:      container.UTSMode("host"),
		NetworkMode:  container.NetworkMode("host"),
		CgroupnsMode: container.CgroupnsModeHost,
		CapAdd:       []string{"ALL"},
		SecurityOpt:  []string{"apparmor=unconfined", "seccomp=unconfined", "label=disable"},
		Mounts: []mount.Mount{
			// A recursive bind, at the daemon's default (private) propagation:
			// rslave would show mounts made on the host after the console
			// started, but the daemon refuses it unless / is already shared or
			// slave, which it is not on a stock host or a WSL distro. An
			// operator who needs the host's live mount table has a better tool
			// anyway — the console shares the host PID namespace, so
			// `nsenter -t 1 -a` enters it directly.
			{Type: mount.TypeBind, Source: "/", Target: consoleHostRoot},
			{Type: mount.TypeBind, Source: socket, Target: dockerSocketPath},
		},
		RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyDisabled},
	}
}

func (e *Engine) consoleLabels(provider *model.SandboxProviderInstance, pool *model.Pool) map[string]string {
	labels := make(map[string]string, len(e.cfg.Labels)+6)
	for key, value := range e.cfg.Labels {
		labels[key] = value
	}
	labels[LabelManaged] = "true"
	labels[LabelPoolConsole] = "true"
	labels[LabelPoolID] = pool.ID
	labels[LabelProjectID] = pool.ProjectID
	if provider != nil {
		labels[LabelProviderInstanceID] = provider.ID
	}
	labels[LabelConsoleConfig] = strconv.Itoa(consoleConfigLayoutVersion)
	return labels
}

// shouldReplaceConsoleContainer reports whether an existing console container
// was built from something this engine no longer wants: another image, or an
// older console layout.
func (e *Engine) shouldReplaceConsoleContainer(existing container.InspectResponse) bool {
	if existing.Config == nil {
		return true
	}
	if strings.TrimSpace(existing.Config.Image) != strings.TrimSpace(e.cfg.Image) {
		return true
	}
	return existing.Config.Labels[LabelConsoleConfig] != strconv.Itoa(consoleConfigLayoutVersion)
}

// removeConsoleContainer removes the pool host's console, if it has one. It is
// part of pool teardown: the console is the one container the engine creates
// that no reconcile would otherwise account for.
func (e *Engine) removeConsoleContainer(ctx context.Context, cli *client.Client, poolID string) error {
	if _, err := cli.ContainerRemove(ctx, ConsoleContainerName(poolID), client.ContainerRemoveOptions{Force: true}); err != nil && !cerrdefs.IsNotFound(err) {
		return err
	}
	return nil
}

// ConsoleContainerName is the deterministic console container name for a pool.
func ConsoleContainerName(poolID string) string {
	name := invalidContainerName.ReplaceAllString(poolID, "-")
	name = strings.Trim(name, "-_.")
	if name == "" {
		name = "pool"
	}
	return "discobox-console-" + name
}

func resizeConsole(ctx context.Context, cli *client.Client, containerID string, rows, cols int) {
	if rows <= 0 || cols <= 0 {
		return
	}
	// A resize that the daemon refuses (the shell exited between attach and
	// resize) is not worth failing the console over: the attach itself reports
	// the same condition as an immediate EOF.
	_, _ = cli.ContainerResize(ctx, containerID, client.ContainerResizeOptions{Height: uint(rows), Width: uint(cols)})
}

// consoleSession is one attach to a pool host console. It owns the Docker
// client lease for the life of the attach, because the hijacked stream is that
// client's connection.
type consoleSession struct {
	client      *client.Client
	lease       *DockerClientLease
	containerID string
	attached    client.ContainerAttachResult
	closeOnce   sync.Once
}

func (c *consoleSession) Read(p []byte) (int, error) { return c.attached.Reader.Read(p) }

func (c *consoleSession) Write(p []byte) (int, error) { return c.attached.Conn.Write(p) }

func (c *consoleSession) Resize(ctx context.Context, rows, cols int) error {
	if rows <= 0 || cols <= 0 {
		return nil
	}
	_, err := c.client.ContainerResize(ctx, c.containerID, client.ContainerResizeOptions{Height: uint(rows), Width: uint(cols)})
	return err
}

// Wait reports the console shell's exit code. It returns only when the console
// container stops, so a caller that has merely detached must bound it: the
// container outliving the attach is the normal case, not the exceptional one.
func (c *consoleSession) Wait(ctx context.Context) (int, error) {
	result := c.client.ContainerWait(ctx, c.containerID, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case err := <-result.Error:
		return 0, err
	case status := <-result.Result:
		if status.Error != nil && strings.TrimSpace(status.Error.Message) != "" {
			return int(status.StatusCode), errors.New(status.Error.Message)
		}
		return int(status.StatusCode), nil
	}
}

// Close detaches. The console container keeps running, which is the point: a
// capture or a trace started in it survives the operator's terminal.
func (c *consoleSession) Close() error {
	c.closeOnce.Do(func() {
		c.attached.Close()
		c.lease.Release()
	})
	return nil
}
