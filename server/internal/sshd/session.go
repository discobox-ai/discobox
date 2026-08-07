package sshd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"

	sandboxgen "github.com/obot-platform/discobox/api/sandboxgen"
	"github.com/obot-platform/discobox/execstream/frame"
	poolagentauth "github.com/obot-platform/discobox/server/internal/auth/poolagent"
	"github.com/obot-platform/discobox/server/internal/model"
	"github.com/obot-platform/discobox/server/internal/sandboxagentclient"
	"github.com/obot-platform/discobox/server/internal/transport"
)

// sftpServerPath is where Debian's openssh-sftp-server package (already
// installed by sandbox-agent/Dockerfile) puts the sftp-server binary. sshd
// is not installed; this is the only file the "ship sftp-server" consequence
// in ADR 0024 needs.
const sftpServerPath = "/usr/lib/openssh/sftp-server"

// allowedSessionEnvName restricts SSH "env" requests to the row ADR 0024 §2
// specifies. Anything else is silently dropped rather than erroring, since
// clients routinely offer more than a server accepts.
func allowedSessionEnvName(name string) bool {
	return name == "TERM" || name == "LANG" || strings.HasPrefix(name, "LC_")
}

// Wire-format structs mirroring golang.org/x/crypto/ssh's unexported request
// payloads (RFC 4254 §§6.2, 6.7, 6.9, 6.10). ssh.Unmarshal/Marshal work on
// exported field order, so these must match the wire layout exactly even
// though the originals cannot be imported.
type ptyReqMsg struct {
	Term     string
	Columns  uint32
	Rows     uint32
	Width    uint32
	Height   uint32
	Modelist string
}

type envReqMsg struct {
	Name  string
	Value string
}

type windowChangeMsg struct {
	Columns uint32
	Rows    uint32
	Width   uint32
	Height  uint32
}

type execReqMsg struct {
	Command string
}

type subsystemReqMsg struct {
	Subsystem string
}

type signalReqMsg struct {
	Signal string
}

type exitStatusMsg struct {
	Status uint32
}

// handleSessionChannel maps an SSH session channel onto the exec primitive
// (ADR 0024 §2): pty-req/shell/exec/subsystem become an exec create+attach,
// window-change/signal become frames, and channel EOF becomes CloseInput.
func (s *Server) handleSessionChannel(ctx context.Context, newChannel ssh.NewChannel, projectID, sandboxID string) {
	channel, requests, err := newChannel.Accept()
	if err != nil {
		return
	}
	defer channel.Close()

	sess := &sshSession{
		server:    s,
		channel:   channel,
		projectID: projectID,
		sandboxID: sandboxID,
		env:       map[string]string{},
	}
	sess.run(ctx, requests)
}

type sshSession struct {
	server    *Server
	channel   ssh.Channel
	projectID string
	sandboxID string

	mu           sync.Mutex
	ptyRequested bool
	term         string
	cols, rows   uint16
	env          map[string]string
	attached     bool
	attachConn   *frameConn
}

// run processes session channel requests until requests closes (the SSH
// library closes it when the channel itself closes), mapping them onto the
// exec primitive per ADR 0024 §2's table.
func (sess *sshSession) run(ctx context.Context, requests <-chan *ssh.Request) {
	for req := range requests {
		switch req.Type {
		case "pty-req":
			var m ptyReqMsg
			if err := ssh.Unmarshal(req.Payload, &m); err != nil {
				sess.reply(req, false)
				continue
			}
			sess.mu.Lock()
			sess.ptyRequested = true
			sess.term = m.Term
			sess.cols = uint16(m.Columns)
			sess.rows = uint16(m.Rows)
			sess.mu.Unlock()
			sess.reply(req, true)

		case "env":
			var m envReqMsg
			if err := ssh.Unmarshal(req.Payload, &m); err == nil && allowedSessionEnvName(m.Name) {
				sess.mu.Lock()
				sess.env[m.Name] = m.Value
				sess.mu.Unlock()
			}
			sess.reply(req, true)

		case "shell":
			sess.attach(ctx, req, execTarget{shell: true})

		case "exec":
			var m execReqMsg
			if err := ssh.Unmarshal(req.Payload, &m); err != nil {
				sess.reply(req, false)
				continue
			}
			// SSH's exec carries one opaque command-line string (RFC 4254
			// §6.5) — no client-side quoting/splitting; the sandbox resolves
			// the login shell and runs it with -lc <command>.
			sess.attach(ctx, req, execTarget{shell: true, shellCommandLine: m.Command})

		case "subsystem":
			var m subsystemReqMsg
			if err := ssh.Unmarshal(req.Payload, &m); err != nil || m.Subsystem != "sftp" {
				sess.reply(req, false)
				continue
			}
			// sftp is a binary protocol over stdio: never a pty, matching
			// the ADR's mapping table exactly.
			sess.attach(ctx, req, execTarget{command: []string{sftpServerPath}, noPTY: true})

		case "window-change":
			var m windowChangeMsg
			if err := ssh.Unmarshal(req.Payload, &m); err == nil {
				if conn := sess.currentAttachConn(); conn != nil {
					if payload, err := frame.EncodeResize(uint16(m.Columns), uint16(m.Rows)); err == nil {
						_ = conn.WriteFrame(frame.Resize, payload)
					}
				}
			}
			// window-change conventionally carries WantReply=false; honor it
			// either way rather than assuming.
			sess.reply(req, true)

		case "signal":
			var m signalReqMsg
			if err := ssh.Unmarshal(req.Payload, &m); err == nil {
				if conn := sess.currentAttachConn(); conn != nil {
					_ = conn.WriteFrame(frame.Signal, []byte(m.Signal))
				}
			}
			sess.reply(req, true)

		default:
			sess.reply(req, false)
		}
	}
}

func (sess *sshSession) reply(req *ssh.Request, ok bool) {
	if req.WantReply {
		_ = req.Reply(ok, nil)
	}
}

func (sess *sshSession) currentAttachConn() *frameConn {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sess.attachConn
}

// execTarget is the shell/exec/subsystem dispatch's parsed intent, translated
// into a CreateSandboxExecRequest by buildCreateExecRequest.
type execTarget struct {
	shell            bool
	shellCommandLine string
	command          []string
	noPTY            bool
}

// attach is the shell/exec/subsystem dispatch (ADR 0024 §2). Only one is
// legal per session channel, matching ordinary SSH semantics. It replies to
// req only once the exec exists and the attach websocket is open, so a
// create failure surfaces as SSH's ordinary "channel request failed" instead
// of a silently hanging session.
func (sess *sshSession) attach(ctx context.Context, req *ssh.Request, target execTarget) {
	sess.mu.Lock()
	if sess.attached {
		sess.mu.Unlock()
		sess.reply(req, false)
		return
	}
	sess.attached = true
	ptyRequested := sess.ptyRequested && !target.noPTY
	term, cols, rows := sess.term, sess.cols, sess.rows
	env := make(map[string]string, len(sess.env))
	for k, v := range sess.env {
		env[k] = v
	}
	sess.mu.Unlock()

	// The exact same choke point an API caller goes through: authorization
	// against the connection's principal happens here, not anywhere in sshd.
	lease, sandboxModel, err := sess.server.sandboxes.AcquireSandboxHTTPClient(ctx, sess.projectID, sess.sandboxID,
		[]string{poolagentauth.ScopeExecRead, poolagentauth.ScopeExecWrite})
	if err != nil {
		sess.server.logger.Warn("ssh exec authorize failed", "project", sess.projectID, "sandbox", sess.sandboxID, "error", err)
		sess.reply(req, false)
		return
	}

	execReq := buildCreateExecRequest(target, ptyRequested, cols, rows, term, env)
	conn, err := createAndDialExec(ctx, lease, sandboxModel, execReq)
	if err != nil {
		lease.Release()
		sess.server.logger.Warn("ssh exec attach failed", "project", sess.projectID, "sandbox", sess.sandboxID, "error", err)
		sess.reply(req, false)
		return
	}

	sess.mu.Lock()
	sess.attachConn = conn
	sess.mu.Unlock()
	sess.reply(req, true)

	go sess.pump(conn, lease)
}

func buildCreateExecRequest(target execTarget, ptyRequested bool, cols, rows uint16, term string, env map[string]string) sandboxgen.CreateSandboxExecRequest {
	req := sandboxgen.CreateSandboxExecRequest{}
	if len(target.command) > 0 {
		req.Command = target.command
	} else {
		req.Shell = sandboxgen.NewOptBool(true)
		if target.shellCommandLine != "" {
			req.ShellCommandLine = sandboxgen.NewOptString(target.shellCommandLine)
		}
	}
	req.Tty = sandboxgen.NewOptBool(ptyRequested)
	if ptyRequested {
		if cols > 0 {
			req.Cols = sandboxgen.NewOptInt(int(cols))
		}
		if rows > 0 {
			req.Rows = sandboxgen.NewOptInt(int(rows))
		}
	}
	if term != "" {
		if _, ok := env["TERM"]; !ok {
			env["TERM"] = term
		}
	}
	if len(env) > 0 {
		req.Env = sandboxgen.NewOptCreateSandboxExecRequestEnv(env)
	}
	return req
}

// createAndDialExec POSTs the exec create request and dials its attach
// websocket, both against the pool-agent target the lease resolves to — the
// same chain sandboxAgentTerminalProxyHandler uses, minus the server's own
// outer HTTP hop (ADR 0024 §1).
func createAndDialExec(ctx context.Context, lease *transport.HTTPClientLease, sandboxModel *model.Sandbox, execReq sandboxgen.CreateSandboxExecRequest) (*frameConn, error) {
	client := sandboxagentclient.HTTPClient(lease)

	createURL, err := sandboxagentclient.TargetURL(lease.BaseURL, sandboxModel.ProjectID, sandboxModel.PoolID, sandboxModel.ID, "/execs")
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(&execReq)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, createURL.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("create exec: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		return nil, fmt.Errorf("create exec: %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	var created sandboxgen.CreateSandboxExecResponse
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		return nil, fmt.Errorf("decode create-exec response: %w", err)
	}

	attachURL, err := sandboxagentclient.TargetURL(lease.BaseURL, sandboxModel.ProjectID, sandboxModel.PoolID, sandboxModel.ID,
		"/execs/"+url.PathEscape(created.Exec.ID)+"/attach")
	if err != nil {
		return nil, err
	}
	switch attachURL.Scheme {
	case "https":
		attachURL.Scheme = "wss"
	default:
		attachURL.Scheme = "ws"
	}

	wsConn, wsResp, err := websocket.Dial(ctx, attachURL.String(), &websocket.DialOptions{HTTPClient: client})
	if err != nil {
		if wsResp != nil && wsResp.Body != nil {
			defer wsResp.Body.Close()
			data, _ := io.ReadAll(io.LimitReader(wsResp.Body, 64*1024))
			return nil, fmt.Errorf("attach exec: %s: %s", wsResp.Status, strings.TrimSpace(string(data)))
		}
		return nil, fmt.Errorf("attach exec: %w", err)
	}
	netConn := websocket.NetConn(ctx, wsConn, websocket.MessageBinary)
	return &frameConn{conn: netConn}, nil
}

// pump bridges the SSH channel and the exec attach connection until the exec
// exits or either side closes, then releases lease. Per ADR 0008 this glue is
// its own small type at this transport boundary, not shared with the CLI's.
func (sess *sshSession) pump(conn *frameConn, lease *transport.HTTPClientLease) {
	defer lease.Release()
	defer conn.Close()

	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := sess.channel.Read(buf)
			if n > 0 {
				if werr := conn.WriteFrame(frame.Input, buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					// The SSH client is done sending, not done reading:
					// CloseInput, not a full close — Stdout/Stderr keep
					// flowing until the exec itself exits.
					_ = conn.WriteFrame(frame.CloseInput, nil)
				}
				return
			}
		}
	}()

	for {
		f, err := conn.ReadFrame()
		if err != nil {
			_ = sess.channel.Close()
			return
		}
		switch f.Type {
		case frame.Stdout:
			if _, err := sess.channel.Write(f.Payload); err != nil {
				_ = sess.channel.Close()
				return
			}
		case frame.Stderr:
			// x/crypto/ssh's extended-data type 1 is exactly Stderr(); per
			// sandbox-agent/DESIGN.md a TTY exec never emits this frame, so
			// this path only ever runs for non-TTY execs.
			if _, err := sess.channel.Stderr().Write(f.Payload); err != nil {
				_ = sess.channel.Close()
				return
			}
		case frame.Exit:
			exit, _ := frame.DecodeExit(f.Payload)
			sess.sendExitStatus(exit)
			_ = sess.channel.Close()
			return
		case frame.Error:
			_ = sess.channel.Close()
			return
		}
	}
}

// sendExitStatus always sends "exit-status", never "exit-signal": the shim
// already converts a signal death to the shell convention (128+signum,
// sandbox-agent/DESIGN.md), so ExitPayload never carries a bare signal name
// to build an exit-signal message from.
func (sess *sshSession) sendExitStatus(exit frame.ExitPayload) {
	var code int64
	switch {
	case exit.ExitCode != nil:
		code = *exit.ExitCode
	case exit.Status != "exited":
		code = 1
	}
	_, _ = sess.channel.SendRequest("exit-status", false, ssh.Marshal(&exitStatusMsg{Status: uint32(code)}))
}

// frameConn adapts a net.Conn (here, a websocket wrapped by
// websocket.NetConn) to frame-oriented read/write. Concurrent writers (the
// stdin pump plus window-change/signal handlers) share one mutex-guarded
// connection.
type frameConn struct {
	conn io.ReadWriteCloser
	mu   sync.Mutex
}

func (c *frameConn) ReadFrame() (frame.Frame, error) { return frame.Read(c.conn) }

func (c *frameConn) WriteFrame(typ byte, payload []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return frame.Write(c.conn, typ, payload)
}

func (c *frameConn) Close() error { return c.conn.Close() }
