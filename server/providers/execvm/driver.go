// Package execvm implements a VM driver that delegates VM CRUD and
// connection resolution to an external command, so a worker backend can be a
// shell script.
//
// The command is invoked once per operation as:
//
//	<command> <op> <worker-id>
//
// with the environment of the control plane plus:
//
//	DISCOBOX_WORKER_ID    the worker ID (same as argv)
//	DISCOBOX_VM_NAME      suggested instance name (ensure-vm only)
//	DISCOBOX_VM_METADATA  JSON object of labels/tags (ensure-vm only)
//
// Operations and their stdout contracts:
//
//	ensure-vm        JSON {"id":"...","status":"created|running|stopped|failed","address":"..."}
//	                 Idempotent create/start of the worker's VM.
//	inspect-vm       Same JSON. Exit code 3 when no VM exists for the worker.
//	delete-vm        No output. Must succeed when the VM is already gone.
//	docker-endpoint  One line: how to reach the VM's Docker daemon:
//	                 unix:///path, tcp://host:port, or ssh://[user@]host[:port]
//	                 (ssh endpoints use the provider's configured private key).
//	harness-endpoint   One line: http(s)://host:port of the worker-agent API.
//
// A non-zero exit is an error and stderr is included in the failure message.
// The engine owns Docker readiness waiting, so ensure-vm may return before
// the VM's Docker daemon is up.
package execvm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"

	sandbox "github.com/obot-platform/discobox/server/internal/sandbox"
	"github.com/obot-platform/discobox/server/internal/transport"
	"github.com/obot-platform/discobox/server/providers/dockerworker"
	"github.com/obot-platform/discobox/server/providers/dockerworker/sshdocker"
)

const (
	opEnsureVM       = "ensure-vm"
	opInspectVM      = "inspect-vm"
	opDeleteVM       = "delete-vm"
	opDockerEndpoint = "docker-endpoint"
	opAgentEndpoint  = "harness-endpoint"

	// notFoundExitCode is the documented inspect-vm exit code for "no VM".
	notFoundExitCode = 3

	maxCommandOutput = 1 << 20
)

// DriverConfig configures an exec VM driver.
type DriverConfig struct {
	// Command is the executable plus fixed leading arguments.
	Command []string
	// SSHUser and SSHPrivateKey authenticate ssh:// docker endpoints.
	SSHUser       string
	SSHPrivateKey string
}

// Driver shells out to the configured command for every VM operation.
type Driver struct {
	command []string
	ssh     *sshdocker.Dialer
}

// NewDriver creates an exec VM driver.
func NewDriver(cfg DriverConfig) (*Driver, error) {
	command := trimCommand(cfg.Command)
	if len(command) == 0 {
		return nil, errors.New("exec driver command is required")
	}
	ssh, err := sshdocker.New(cfg.SSHUser, cfg.SSHPrivateKey)
	if err != nil {
		return nil, err
	}
	return &Driver{command: command, ssh: ssh}, nil
}

func trimCommand(command []string) []string {
	out := make([]string, 0, len(command))
	for _, part := range command {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func (d *Driver) Close() error {
	return nil
}

func (d *Driver) EnsureVM(ctx context.Context, workerID string, spec dockerworker.VMSpec) (*dockerworker.VMInfo, error) {
	metadata, err := json.Marshal(spec.Metadata)
	if err != nil {
		return nil, err
	}
	out, err := d.run(ctx, opEnsureVM, workerID,
		"DISCOBOX_VM_NAME="+spec.Name,
		"DISCOBOX_VM_METADATA="+string(metadata),
	)
	if err != nil {
		return nil, err
	}
	return parseVMInfo(opEnsureVM, out)
}

func (d *Driver) DeleteVM(ctx context.Context, workerID string) error {
	_, err := d.run(ctx, opDeleteVM, workerID)
	return err
}

func (d *Driver) InspectVM(ctx context.Context, workerID string) (*dockerworker.VMInfo, error) {
	out, err := d.run(ctx, opInspectVM, workerID)
	if err != nil {
		return nil, err
	}
	return parseVMInfo(opInspectVM, out)
}

func (d *Driver) AcquireDockerClient(ctx context.Context, workerID string) (*dockerworker.DockerClientLease, error) {
	out, err := d.run(ctx, opDockerEndpoint, workerID)
	if err != nil {
		return nil, err
	}
	endpoint := firstLine(out)
	if endpoint == "" {
		return nil, fmt.Errorf("exec driver %s returned no endpoint", opDockerEndpoint)
	}
	if strings.HasPrefix(endpoint, "ssh://") {
		target, err := sshdocker.ParseURL(endpoint)
		if err != nil {
			return nil, err
		}
		return d.ssh.AcquireDockerClient(ctx, target)
	}
	cli, err := dockerworker.NewDockerClientForHost(endpoint)
	if err != nil {
		return nil, fmt.Errorf("exec driver docker endpoint %q: %w", endpoint, err)
	}
	return dockerworker.NewDockerClientLease(cli, func() { _ = cli.Close() }), nil
}

func (d *Driver) AcquireWorkerAgentClient(ctx context.Context, workerID string) (*transport.HTTPClientLease, error) {
	out, err := d.run(ctx, opAgentEndpoint, workerID)
	if err != nil {
		return nil, err
	}
	endpoint := firstLine(out)
	if !strings.HasPrefix(endpoint, "http://") && !strings.HasPrefix(endpoint, "https://") {
		return nil, fmt.Errorf("exec driver %s returned %q, want an http(s) URL", opAgentEndpoint, endpoint)
	}
	return transport.NewHTTPClientLeaseWithBaseURL(http.DefaultClient, endpoint, nil), nil
}

func (d *Driver) run(ctx context.Context, op, workerID string, extraEnv ...string) ([]byte, error) {
	if strings.TrimSpace(workerID) == "" {
		return nil, errors.New("worker ID is required")
	}
	args := append(append([]string(nil), d.command[1:]...), op, workerID)
	cmd := exec.CommandContext(ctx, d.command[0], args...) //nolint:gosec // Running an operator-configured command is this driver's purpose; it comes from provider config, not request input.
	cmd.Env = append(os.Environ(), "DISCOBOX_WORKER_ID="+workerID)
	cmd.Env = append(cmd.Env, extraEnv...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = newLimitedWriter(&stdout)
	cmd.Stderr = newLimitedWriter(&stderr)
	err := cmd.Run()
	if err == nil {
		return stdout.Bytes(), nil
	}
	var exitErr *exec.ExitError
	if op == opInspectVM && errors.As(err, &exitErr) && exitErr.ExitCode() == notFoundExitCode {
		return nil, sandbox.ErrNotFound
	}
	message := strings.TrimSpace(stderr.String())
	if message == "" {
		message = err.Error()
	}
	return nil, fmt.Errorf("exec driver %s %s failed: %s", d.command[0], op, message)
}

func parseVMInfo(op string, out []byte) (*dockerworker.VMInfo, error) {
	var payload struct {
		ID      string `json:"id"`
		Status  string `json:"status"`
		Address string `json:"address"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &payload); err != nil {
		return nil, fmt.Errorf("exec driver %s output is not valid JSON: %w", op, err)
	}
	if payload.ID == "" {
		return nil, fmt.Errorf("exec driver %s output has no id", op)
	}
	status, err := parseStatus(payload.Status)
	if err != nil {
		return nil, fmt.Errorf("exec driver %s: %w", op, err)
	}
	return &dockerworker.VMInfo{ID: payload.ID, Status: status, Address: payload.Address}, nil
}

func parseStatus(value string) (sandbox.Status, error) {
	switch status := sandbox.Status(strings.TrimSpace(value)); status {
	case sandbox.StatusCreated, sandbox.StatusRunning, sandbox.StatusStopped, sandbox.StatusFailed:
		return status, nil
	default:
		return "", fmt.Errorf("unknown VM status %q", value)
	}
}

func firstLine(out []byte) string {
	line, _, _ := strings.Cut(string(out), "\n")
	return strings.TrimSpace(line)
}

type limitedWriter struct {
	buf *bytes.Buffer
}

func newLimitedWriter(buf *bytes.Buffer) *limitedWriter {
	return &limitedWriter{buf: buf}
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	remaining := maxCommandOutput - w.buf.Len()
	if remaining > 0 {
		if len(p) > remaining {
			w.buf.Write(p[:remaining])
		} else {
			w.buf.Write(p)
		}
	}
	// Report full consumption so the child process is never blocked or broken
	// by the output cap.
	return len(p), nil
}
