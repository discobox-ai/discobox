package provisioner

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/system"
	"github.com/moby/moby/client"

	"github.com/discobox-ai/discobox/pkg/imagecache"
)

// ContainerGuest is a running container as disco-vm's machine.Guest: its
// copy and its exec, so Provision places a tree in a container and runs a
// command there as it does in a machine (ADR 26-10-24-953 §2). A Docker pool's
// box is made over it, and a Docker pool's snapshot build provisioned over it
// (ADR 26-11-03-986 §1).
type ContainerGuest struct {
	Client    *client.Client
	Container string
	// Env is supplied independently on every exec; runc consumes the create marker.
	Env []string
	// Stdin declares commands that consume input. Without it Docker closes the
	// process input itself; half-closing a transport that carries only output is
	// unnecessary and loses that output on libkrun's host VSOCK mapping.
	Stdin bool
}

var _ machine.Guest = ContainerGuest{}

func (g ContainerGuest) Exec(ctx context.Context, req machine.ExecRequest) (machine.Process, error) {
	created, err := g.Client.ExecCreate(ctx, g.Container, client.ExecCreateOptions{
		User:         req.User,
		TTY:          req.TTY,
		AttachStdin:  g.Stdin,
		AttachStdout: true,
		AttachStderr: true,
		Env:          append(append([]string{}, g.Env...), req.Env...),
		WorkingDir:   req.Dir,
		Cmd:          req.Argv,
	})
	if err != nil {
		return nil, err
	}
	attached, err := g.Client.ExecAttach(ctx, created.ID, client.ExecAttachOptions{TTY: req.TTY})
	if err != nil {
		return nil, err
	}
	return &containerProcess{cli: g.Client, id: created.ID, tty: req.TTY, stdin: g.Stdin, attached: attached}, nil
}

func (g ContainerGuest) CopyTo(ctx context.Context, dir string, tarStream io.Reader) error {
	_, err := g.Client.CopyToContainer(ctx, g.Container, client.CopyToContainerOptions{DestinationPath: dir, Content: tarStream})
	return err
}

func (g ContainerGuest) CopyFrom(ctx context.Context, path string) (io.ReadCloser, error) {
	result, err := g.Client.CopyFromContainer(ctx, g.Container, client.CopyFromContainerOptions{SourcePath: path})
	if err != nil {
		return nil, err
	}
	return result.Content, nil
}

func (g ContainerGuest) Info(ctx context.Context) (machine.GuestInfo, error) {
	platform, err := ContainerPlatform(ctx, g.Client)
	if err != nil {
		return machine.GuestInfo{}, err
	}
	return machine.GuestInfo{OS: platform.OS, Arch: platform.Architecture, Hostname: g.Container}, nil
}

func (g ContainerGuest) Shutdown(ctx context.Context, reboot bool) error {
	if reboot {
		_, err := g.Client.ContainerRestart(ctx, g.Container, client.ContainerRestartOptions{})
		return err
	}
	_, err := g.Client.ContainerStop(ctx, g.Container, client.ContainerStopOptions{})
	return err
}

// ContainerPlatform is the platform of the daemon a container runs on, which
// an agent bundle's layer is chosen for.
func ContainerPlatform(ctx context.Context, cli *client.Client) (imagecache.Platform, error) {
	info, err := cli.Info(ctx, client.InfoOptions{})
	if err != nil {
		return imagecache.Platform{}, fmt.Errorf("read the daemon's platform: %w", err)
	}
	platform, ok := DaemonPlatform(info.Info)
	if !ok {
		return imagecache.Platform{}, fmt.Errorf("the daemon reports no platform the agent is built for (%s/%s)", info.Info.OSType, info.Info.Architecture)
	}
	return platform, nil
}

// DaemonPlatform is the platform a daemon's info names, in the terms an image
// index uses.
func DaemonPlatform(info system.Info) (imagecache.Platform, bool) {
	architecture := map[string]string{
		"x86_64": "amd64", "amd64": "amd64",
		"aarch64": "arm64", "arm64": "arm64",
	}[strings.ToLower(info.Architecture)]
	if architecture == "" || info.OSType == "" {
		return imagecache.Platform{}, false
	}
	return imagecache.Platform{OS: info.OSType, Architecture: architecture}, true
}

// containerProcess is one exec in a container.
type containerProcess struct {
	cli      *client.Client
	id       string
	tty      bool
	stdin    bool
	attached client.ExecAttachResult
}

func (p *containerProcess) Write(b []byte) (int, error) {
	if !p.stdin {
		return 0, fmt.Errorf("container exec does not accept stdin")
	}
	return p.attached.Conn.Write(b)
}

func (p *containerProcess) CloseStdin() error {
	if !p.stdin {
		return nil
	}
	return p.attached.CloseWrite()
}

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
