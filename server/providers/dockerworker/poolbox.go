package dockerworker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"

	"github.com/discobox-ai/discobox/pkg/agentlayer"
	"github.com/discobox-ai/discobox/pkg/imagecache"
	"github.com/discobox-ai/discobox/pkg/layout"
	poolagent "github.com/discobox-ai/discobox/pool-agent"
	"github.com/discobox-ai/discobox/pool-agent/bundle"
	"github.com/discobox-ai/discobox/pool-agent/provisioner"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/providers/poolruntime"
)

// A Docker pool is made the way a pool makes a sandbox (ADR 26-10-24-953
// §§2, 4): its container runs the sandbox base image, systemd as PID 1, and
// this places the agent and the pool's pins in it, has the box make its key
// and enrolls it, places the bootstrap, and runs `discobox-provision apply`,
// which applies the pool bundle's features — the agent and its proxy among
// them. Placing and running are the provisioner's, over the container as a
// machine.Guest, as a machine's are over disco-vm's.

// poolBoxRecord is what the engine placed in a pool's box, kept with the
// pool's runtime state, the server's own record: a box whose bundles moved
// since is provisioned again, in place. The agent alone moving provisions
// nothing (ADR 26-10-24-953 §5): it is the provisioner, not what the pool
// runs.
type poolBoxRecord struct {
	Agent   string   `json:"agent,omitempty"`
	Bundles []string `json:"bundles,omitempty"`
}

func (r poolBoxRecord) sameBundles(box sandbox.PoolBox) bool {
	if len(r.Bundles) != len(box.Bundles) {
		return false
	}
	for i, pin := range box.Bundles {
		if r.Bundles[i] != pin.Name+"@"+pin.Digest {
			return false
		}
	}
	return true
}

func recordOf(box sandbox.PoolBox) poolBoxRecord {
	record := poolBoxRecord{Agent: box.Agent.Name + "@" + box.Agent.Digest}
	for _, pin := range box.Bundles {
		record.Bundles = append(record.Bundles, pin.Name+"@"+pin.Digest)
	}
	return record
}

// provisionTimeout bounds one provisioning of a pool's box: the pool
// bundle's fetch and every one of its steps, blocking.
const provisionTimeout = 15 * time.Minute

// provisionPoolBox makes the running container a pool's box, or makes it so
// again: it places the agent and pins, enrolls the key the box makes or
// already holds, places the bootstrap and the agent's environment, and
// applies the pool's bundles. Every step is idempotent, so a box provisioned
// before is provisioned again in place, its key the same.
func (e *Engine) provisionPoolBox(ctx context.Context, cli *client.Client, containerID string, maker poolruntime.PoolMaker, bootstrap poolagent.Bootstrap, box sandbox.PoolBox) (poolBoxRecord, error) {
	ctx, cancel := context.WithTimeout(ctx, provisionTimeout)
	defer cancel()
	guest := containerGuest{cli: cli, container: containerID}
	agentDir, err := os.MkdirTemp("", "discobox-pool-agent-")
	if err != nil {
		return poolBoxRecord{}, err
	}
	defer func() { _ = os.RemoveAll(agentDir) }()
	agentPath := filepath.Join(agentDir, "discobox-sandbox-agent")
	layer, err := box.OpenAgentLayer(ctx)
	if err != nil {
		return poolBoxRecord{}, fmt.Errorf("read the agent bundle %s: %w", box.Agent.Name, err)
	}
	err = agentlayer.ExtractAgent(layer, agentPath)
	layer.Close()
	if err != nil {
		return poolBoxRecord{}, fmt.Errorf("the agent bundle %s@%s: %w", box.Agent.Name, box.Agent.Digest, err)
	}
	info, err := os.Stat(agentPath)
	if err != nil {
		return poolBoxRecord{}, err
	}
	pins := make(agentlayer.Pins, 0, len(box.Bundles))
	for _, pin := range box.Bundles {
		pins = append(pins, agentlayer.Pin{Name: pin.Name, Digest: pin.Digest})
	}
	agent := agentlayer.Agent{
		Digest: box.AgentLayer,
		Size:   info.Size(),
		Open:   func() (io.ReadCloser, error) { return os.Open(agentPath) },
	}
	var placement bytes.Buffer
	if err := agentlayer.Archive(&placement, agent, false, pins, nil); err != nil {
		return poolBoxRecord{}, err
	}
	keyPath := layout.Container().PoolIdentityKey(bootstrap.ProjectID, bootstrap.PoolID)
	identity, err := provisioner.Provision(ctx, guest, provisioner.Plan{
		Dir:     "/",
		Tree:    &placement,
		Command: machine.ExecRequest{Argv: []string{agentlayer.AgentPath, "provision", "identity", "--key", keyPath}},
	})
	if err != nil {
		return poolBoxRecord{}, provisionError("make the pool's identity", identity, err)
	}
	publicKey := strings.TrimSpace(string(identity.Stdout))
	if publicKey == "" {
		return poolBoxRecord{}, errors.New("the pool's box answered no identity")
	}
	if err := maker.Enroll(ctx, publicKey); err != nil {
		return poolBoxRecord{}, fmt.Errorf("enroll the pool's identity: %w", err)
	}
	files, err := e.poolBootstrapFiles(bootstrap)
	if err != nil {
		return poolBoxRecord{}, err
	}
	applied, err := provisioner.Provision(ctx, guest, provisioner.Plan{
		Dir:  "/",
		Tree: files,
		Command: machine.ExecRequest{Argv: []string{
			agentlayer.AgentPath, "provision", "apply",
			"--control-plane", bootstrap.ControlPlaneURL,
			"--project", bootstrap.ProjectID,
			"--pool", bootstrap.PoolID,
			"--key", keyPath,
		}},
	})
	if err != nil {
		return poolBoxRecord{}, provisionError("provision the pool's box", applied, err)
	}
	return recordOf(box), nil
}

// provisionError names what failed with what the command wrote last, which
// is where it says why.
func provisionError(what string, result provisioner.Result, err error) error {
	if out := strings.TrimSpace(string(result.Stderr)); out != "" {
		return fmt.Errorf("%s: %w: %s", what, err, lastLines(out, 20))
	}
	return fmt.Errorf("%s: %w", what, err)
}

func lastLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// poolBootstrapFiles is the tree of what the pool agent reads beside its
// bundle: the bootstrap, root's alone (bundle.BootstrapFile), and the policy
// it reads from its environment (bundle.EnvironmentFile).
func (e *Engine) poolBootstrapFiles(bootstrap poolagent.Bootstrap) (io.Reader, error) {
	data, err := json.MarshalIndent(bootstrap, "", "  ")
	if err != nil {
		return nil, err
	}
	var env strings.Builder
	for _, line := range envList(e.poolPolicyEnv()) {
		env.WriteString(line)
		env.WriteByte('\n')
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, dir := range []string{path.Dir(bundle.BootstrapFile)} {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: strings.TrimPrefix(dir, "/") + "/", Mode: 0o755}); err != nil {
			return nil, err
		}
	}
	for _, file := range []struct {
		name string
		data []byte
	}{{bundle.BootstrapFile, data}, {bundle.EnvironmentFile, []byte(env.String())}} {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: strings.TrimPrefix(file.name, "/"), Mode: 0o600, Size: int64(len(file.data))}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(file.data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return &buf, nil
}

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
