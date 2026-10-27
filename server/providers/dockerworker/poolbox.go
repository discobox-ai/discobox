package dockerworker

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"

	"github.com/discobox-ai/discobox/pkg/imagecache"
)

// A Docker pool is made the way a pool makes a sandbox (ADR 26-10-24-953
// §§2, 4): its container runs the sandbox base image, systemd as PID 1, and
// poolruntime.ProvisionBox makes it the pool's box over the container as a
// machine.Guest (containerGuest), as a VM pool's machine is made over
// disco-vm's.

// boxPlatform is the platform of the daemon a pool's box runs on, which the
// agent bundle's layer is chosen for.
func boxPlatform(ctx context.Context, cli *client.Client) (imagecache.Platform, error) {
	info, err := cli.Info(ctx, client.InfoOptions{})
	if err != nil {
		return imagecache.Platform{}, fmt.Errorf("read the pool daemon's platform: %w", err)
	}
	platform, ok := daemonPlatform(info.Info)
	if !ok {
		return imagecache.Platform{}, fmt.Errorf("the pool daemon reports no platform the agent is built for (%s/%s)", info.Info.OSType, info.Info.Architecture)
	}
	return platform, nil
}

// containerGuest is a running container as disco-vm's machine.Guest: its
// copy and its exec, so the provisioner that places a tree in a machine and
// runs a command there does the same in a pool's container (ADR
// 26-10-24-953 §2).
type containerGuest struct {
	cli       *client.Client
	container string
}

var _ machine.Guest = containerGuest{}

func (g containerGuest) Exec(ctx context.Context, req machine.ExecRequest) (machine.Process, error) {
	created, err := g.cli.ExecCreate(ctx, g.container, client.ExecCreateOptions{
		User:         req.User,
		TTY:          req.TTY,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		Env:          req.Env,
		WorkingDir:   req.Dir,
		Cmd:          req.Argv,
	})
	if err != nil {
		return nil, err
	}
	attached, err := g.cli.ExecAttach(ctx, created.ID, client.ExecAttachOptions{TTY: req.TTY})
	if err != nil {
		return nil, err
	}
	return &containerProcess{cli: g.cli, id: created.ID, tty: req.TTY, attached: attached}, nil
}

func (g containerGuest) CopyTo(ctx context.Context, dir string, tarStream io.Reader) error {
	_, err := g.cli.CopyToContainer(ctx, g.container, client.CopyToContainerOptions{DestinationPath: dir, Content: tarStream})
	return err
}

func (g containerGuest) CopyFrom(ctx context.Context, path string) (io.ReadCloser, error) {
	result, err := g.cli.CopyFromContainer(ctx, g.container, client.CopyFromContainerOptions{SourcePath: path})
	if err != nil {
		return nil, err
	}
	return result.Content, nil
}

func (g containerGuest) Info(ctx context.Context) (machine.GuestInfo, error) {
	platform, err := boxPlatform(ctx, g.cli)
	if err != nil {
		return machine.GuestInfo{}, err
	}
	return machine.GuestInfo{OS: platform.OS, Arch: platform.Architecture, Hostname: g.container}, nil
}

func (g containerGuest) Shutdown(ctx context.Context, reboot bool) error {
	if reboot {
		_, err := g.cli.ContainerRestart(ctx, g.container, client.ContainerRestartOptions{})
		return err
	}
	_, err := g.cli.ContainerStop(ctx, g.container, client.ContainerStopOptions{})
	return err
}

// containerProcess is one exec in a container.
type containerProcess struct {
	cli      *client.Client
	id       string
	tty      bool
	attached client.ExecAttachResult
}

func (p *containerProcess) Write(b []byte) (int, error) { return p.attached.Conn.Write(b) }

func (p *containerProcess) CloseStdin() error { return p.attached.CloseWrite() }

func (p *containerProcess) Resize(rows, cols uint16) error {
	_, err := p.cli.ExecResize(context.Background(), p.id, client.ExecResizeOptions{Height: uint(rows), Width: uint(cols)})
	return err
}

// Wait copies the exec's output until it ends, and then reads its exit code,
// which the daemon records just after the stream closes.
func (p *containerProcess) Wait(stdout, stderr io.Writer) (int, error) {
	var err error
	if p.tty {
		_, err = io.Copy(stdout, p.attached.Reader)
	} else {
		_, err = stdcopy.StdCopy(stdout, stderr, p.attached.Reader)
	}
	if err != nil {
		return -1, err
	}
	for range 100 {
		result, err := p.cli.ExecInspect(context.Background(), p.id, client.ExecInspectOptions{})
		if err != nil {
			return -1, err
		}
		if !result.Running {
			return result.ExitCode, nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return -1, fmt.Errorf("exec %s did not report its exit after its output ended", p.id)
}

func (p *containerProcess) Close() error {
	p.attached.Close()
	return nil
}
