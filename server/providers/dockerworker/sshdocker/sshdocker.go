// Package sshdocker adapts SSH connections into Docker API client leases for
// VM drivers whose in-VM Docker daemon is reached over SSH, such as
// DigitalOcean, EC2, or script-managed VMs.
package sshdocker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/discobox-ai/discobox/server/providers/dockerworker"
)

const (
	// DefaultUser is the SSH user assumed when the target does not name one.
	DefaultUser = "root"

	defaultPort      = 22
	connectTimeout   = 15 * time.Second
	remoteSocketPath = "/var/run/docker.sock"
)

// Target addresses one VM's SSH endpoint. Empty fields fall back to the
// dialer's defaults.
type Target struct {
	Host string
	User string
	Port int
}

// ParseURL converts an ssh://[user@]host[:port] URL into a Target.
func ParseURL(value string) (Target, error) {
	parsed, err := url.Parse(value)
	if err != nil {
		return Target{}, err
	}
	if parsed.Scheme != "ssh" {
		return Target{}, fmt.Errorf("ssh docker endpoint %q must use the ssh scheme", value)
	}
	if parsed.Hostname() == "" {
		return Target{}, fmt.Errorf("ssh docker endpoint %q has no host", value)
	}
	target := Target{Host: parsed.Hostname(), User: parsed.User.Username()}
	if portValue := parsed.Port(); portValue != "" {
		port, err := strconv.Atoi(portValue)
		if err != nil {
			return Target{}, fmt.Errorf("ssh docker endpoint %q has an invalid port: %w", value, err)
		}
		target.Port = port
	}
	return target, nil
}

// Dialer opens SSH connections to worker VMs and adapts them into Docker API
// client leases that dial the in-VM Unix socket.
type Dialer struct {
	user   string
	signer ssh.Signer
}

// New parses the SSH credentials when configured. A missing key is not a
// construction error so provider instances still resolve for catalog and pool
// operations; AcquireDockerClient reports the missing key when Docker access
// is needed.
func New(user, privateKey string) (*Dialer, error) {
	dialer := &Dialer{user: user}
	if dialer.user == "" {
		dialer.user = DefaultUser
	}
	if strings.TrimSpace(privateKey) == "" {
		return dialer, nil
	}
	signer, err := ssh.ParsePrivateKey([]byte(privateKey))
	if err != nil {
		return nil, fmt.Errorf("parse ssh private key: %w", err)
	}
	dialer.signer = signer
	return dialer, nil
}

// AcquireDockerClient opens an SSH connection to the target and returns a
// Docker API client whose connections ride the SSH channel to the in-VM
// Docker socket. Releasing the lease closes both.
func (d *Dialer) AcquireDockerClient(ctx context.Context, target Target) (*dockerworker.DockerClientLease, error) {
	sshClient, err := d.Dial(ctx, target)
	if err != nil {
		return nil, err
	}
	dockerClient, err := dockerworker.NewDockerClientForDialer(func(dialCtx context.Context, _, _ string) (net.Conn, error) {
		return sshClient.DialContext(dialCtx, "unix", remoteSocketPath)
	})
	if err != nil {
		_ = sshClient.Close()
		return nil, err
	}
	return dockerworker.NewDockerClientLease(dockerClient, func() {
		_ = dockerClient.Close()
		_ = sshClient.Close()
	}), nil
}

// Dial opens an SSH client connection to the target.
//
// Worker VMs are created fresh by their provider, so there is no prior host
// key to pin; host key verification is skipped and transport trust rests on
// the key-authenticated SSH channel.
func (d *Dialer) Dial(ctx context.Context, target Target) (*ssh.Client, error) {
	if d.signer == nil {
		return nil, errors.New("ssh private key is required to reach the worker docker daemon; set sshPrivateKey or sshPrivateKeyEnv")
	}
	if strings.TrimSpace(target.Host) == "" {
		return nil, errors.New("ssh target host is required")
	}
	user := target.User
	if user == "" {
		user = d.user
	}
	port := target.Port
	if port <= 0 {
		port = defaultPort
	}
	addr := net.JoinHostPort(target.Host, strconv.Itoa(port))
	conn, err := (&net.Dialer{Timeout: connectTimeout}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	config := &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(d.signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // Fresh worker VMs have no pinnable host key; see comment above.
		Timeout:         connectTimeout,
	}
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, addr, config)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return ssh.NewClient(sshConn, chans, reqs), nil
}
