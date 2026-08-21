package sshd

import (
	"context"
	"errors"
	"io"
	"net/url"
	"strconv"

	"golang.org/x/crypto/ssh"

	"github.com/discobox-ai/discobox/execstream/frame"
	poolagentauth "github.com/discobox-ai/discobox/server/internal/auth/poolagent"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/sandboxagentclient"
	"github.com/discobox-ai/discobox/server/internal/transport"
)

// directTCPIPMsg is RFC 4254 §7.2's direct-tcpip channel-open payload.
type directTCPIPMsg struct {
	HostToConnect     string
	PortToConnect     uint32
	OriginatorAddress string
	OriginatorPort    uint32
}

// handleDirectTCPIPChannel serves an SSH direct-tcpip channel (ssh -L / -D)
// by dialing host:port from inside the target sandbox's network namespace
// (ADR 0024 §3), through the new sandbox-agent /tcp/attach endpoint reached
// via the same lease chain and pool-agent target as exec attaches.
func (s *Server) handleDirectTCPIPChannel(ctx context.Context, newChannel ssh.NewChannel, projectID, sandboxID string) {
	var payload directTCPIPMsg
	if err := ssh.Unmarshal(newChannel.ExtraData(), &payload); err != nil {
		_ = newChannel.Reject(ssh.ConnectionFailed, "malformed direct-tcpip request")
		return
	}

	lease, sandboxModel, err := s.sandboxes.AcquireSandboxHTTPClient(ctx, projectID, sandboxID, []string{poolagentauth.ScopeTCPConnect})
	if err != nil {
		s.logger.Warn("ssh direct-tcpip authorize failed", "project", projectID, "sandbox", sandboxID, "error", err)
		_ = newChannel.Reject(ssh.ConnectionFailed, err.Error())
		return
	}

	conn, err := dialTCPTunnel(ctx, lease, sandboxModel, payload.HostToConnect, payload.PortToConnect)
	if err != nil {
		lease.Release()
		s.logger.Warn("ssh direct-tcpip dial failed", "project", projectID, "sandbox", sandboxID,
			"host", payload.HostToConnect, "port", payload.PortToConnect, "error", err)
		// Reject before accepting, matching what a real sshd does for a
		// refused -L target: the client sees a clean channel-open failure.
		_ = newChannel.Reject(ssh.ConnectionFailed, err.Error())
		return
	}

	channel, requests, err := newChannel.Accept()
	if err != nil {
		lease.Release()
		conn.Close()
		return
	}
	go ssh.DiscardRequests(requests)

	go pumpDirectTCPIP(channel, conn, lease)
}

// dialTCPTunnel opens the attach websocket for a direct-tcpip channel against
// the sandbox-agent's /tcp/attach endpoint, reached through the same
// pool-agent target exec attaches use.
func dialTCPTunnel(ctx context.Context, lease *transport.HTTPClientLease, sandboxModel *model.Sandbox, host string, port uint32) (*frameConn, error) {
	client := sandboxagentclient.HTTPClient(lease)
	attachURL, err := sandboxagentclient.TargetURL(lease.BaseURL, sandboxModel.ProjectID, sandboxModel.PoolID, sandboxModel.ID, "/tcp/attach")
	if err != nil {
		return nil, err
	}
	attachURL.RawQuery = url.Values{
		"host": {host},
		"port": {strconv.FormatUint(uint64(port), 10)},
	}.Encode()
	return dialFrameWebSocket(ctx, client, attachURL, "direct-tcpip attach")
}

// pumpDirectTCPIP bridges the SSH channel and the tunnel's attach connection
// until either side closes, then releases lease. A TCP pipe has no exit code,
// so unlike the session-channel pump there is no Exit frame to wait for —
// either side ending the byte stream ends the tunnel.
func pumpDirectTCPIP(channel ssh.Channel, conn *frameConn, lease *transport.HTTPClientLease) {
	defer lease.Release()
	defer conn.Close()
	defer channel.Close()

	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := channel.Read(buf)
			if n > 0 {
				if werr := conn.WriteFrame(frame.Input, buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					_ = conn.WriteFrame(frame.CloseInput, nil)
				}
				return
			}
		}
	}()

	for {
		f, err := conn.ReadFrame()
		if err != nil {
			return
		}
		if f.Type == frame.Stdout {
			if _, err := channel.Write(f.Payload); err != nil {
				return
			}
		}
	}
}
