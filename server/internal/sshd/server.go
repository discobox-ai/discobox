// Package sshd is the SSH control-plane ingress ADR 0024 describes: a
// listener on discobox-server that maps SSH session channels onto the
// existing exec primitive and, for direct-tcpip, a new sandbox-agent TCP
// dial endpoint. It authenticates independently of internal/auth's HTTP
// Authentication/Authorization chain — see DESIGN.md — and, once a
// connection is authenticated, drives services.SandboxService exactly the
// way an HTTP caller would.
package sshd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"

	"golang.org/x/crypto/ssh"

	"github.com/obot-platform/discobox/server/internal/auth"
	poolagentauth "github.com/obot-platform/discobox/server/internal/auth/poolagent"
	"github.com/obot-platform/discobox/server/internal/services"
	"github.com/obot-platform/discobox/server/internal/store"
)

// grantExtension and grantFile/grantProject are carried in ssh.Permissions
// between PublicKeyCallback and the post-handshake connection handler, since
// x/crypto/ssh has no other channel to pass authorization results forward.
const grantExtension = "discobox-grant"

const (
	grantFile    = "file"
	grantProject = "project"
)

const (
	projectExtension = "discobox-project-id"
	sandboxExtension = "discobox-sandbox-id"
	sshKeyExtension  = "discobox-ssh-key-id"
	userIDExtension  = "discobox-user-id"
)

type Options struct {
	HostKey ssh.Signer
	DataDir string
	Store   *store.Store
	// Sandboxes is the exact choke point an HTTP caller goes through:
	// AcquireSandboxHTTPClient authorizes the requested scopes against the
	// connection's principal and leases the pool-agent HTTP client. sshd
	// never bypasses it.
	Sandboxes services.SandboxService
	// DefaultUserID is the Principal.UserID a server-wide authorized_keys
	// match authenticates as — the same default user DefaultUserAuthenticator
	// uses for the HTTP path.
	DefaultUserID string
	Logger        *slog.Logger
}

type Server struct {
	hostKey       ssh.Signer
	dataDir       string
	store         *store.Store
	sandboxes     services.SandboxService
	defaultUserID string
	logger        *slog.Logger
}

func NewServer(opts Options) (*Server, error) {
	if opts.HostKey == nil {
		return nil, errors.New("sshd: host key is required")
	}
	if opts.DataDir == "" {
		return nil, errors.New("sshd: data directory is required")
	}
	if opts.Store == nil {
		return nil, errors.New("sshd: store is required")
	}
	if opts.Sandboxes == nil {
		return nil, errors.New("sshd: sandbox service is required")
	}
	if opts.DefaultUserID == "" {
		return nil, errors.New("sshd: default user ID is required")
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		hostKey:       opts.HostKey,
		dataDir:       opts.DataDir,
		store:         opts.Store,
		sandboxes:     opts.Sandboxes,
		defaultUserID: opts.DefaultUserID,
		logger:        logger,
	}, nil
}

func (s *Server) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	config := &ssh.ServerConfig{
		PublicKeyCallback: s.publicKeyCallback(ctx),
	}
	config.AddHostKey(s.hostKey)

	sshConn, chans, globalReqs, err := ssh.NewServerConn(conn, config)
	if err != nil {
		s.logger.Debug("ssh handshake failed", "remote", conn.RemoteAddr(), "error", err)
		return
	}
	defer sshConn.Close()

	// Reject every global request. This is what refuses tcpip-forward /
	// cancel-tcpip-forward (remote forwarding, ADR 0024 §8's "out of scope")
	// with no special-case code: an unrecognized global request type is
	// simply never accepted.
	go func() {
		for req := range globalReqs {
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
		}
	}()

	principal, projectID, sandboxID := principalFromPermissions(sshConn.Permissions, s.defaultUserID)
	connCtx := auth.WithPrincipal(ctx, principal)

	for newChannel := range chans {
		switch newChannel.ChannelType() {
		case "session":
			go s.handleSessionChannel(connCtx, newChannel, projectID, sandboxID)
		case "direct-tcpip":
			go s.handleDirectTCPIPChannel(connCtx, newChannel, projectID, sandboxID)
		default:
			_ = newChannel.Reject(ssh.UnknownChannelType, fmt.Sprintf("unsupported channel type %q", newChannel.ChannelType()))
		}
	}
}

func (s *Server) publicKeyCallback(ctx context.Context) func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
	return func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		projectID, sandboxID, err := ResolveUsername(ctx, s.store, conn.User())
		if err != nil {
			return nil, errors.New("sandbox not found")
		}
		fingerprint := ssh.FingerprintSHA256(key)

		// Broader grant wins (ADR 0024 §5): a file-layer match always beats a
		// project-scoped one, so check it first and only fall through to the
		// project layer when it doesn't match.
		fileKeys, err := LoadAuthorizedKeys(s.dataDir)
		if err != nil {
			return nil, err
		}
		if _, ok := fileKeys[fingerprint]; ok {
			return &ssh.Permissions{Extensions: map[string]string{
				grantExtension:   grantFile,
				projectExtension: projectID,
				sandboxExtension: sandboxID,
				userIDExtension:  s.defaultUserID,
			}}, nil
		}

		projectKeys, err := s.store.ListSSHKeys(ctx, projectID)
		if err != nil {
			return nil, err
		}
		for _, k := range projectKeys {
			if k.Fingerprint != fingerprint {
				continue
			}
			return &ssh.Permissions{Extensions: map[string]string{
				grantExtension:   grantProject,
				projectExtension: projectID,
				sandboxExtension: sandboxID,
				sshKeyExtension:  k.ID,
				userIDExtension:  k.CreatedBy,
			}}, nil
		}

		return nil, errors.New("no matching authorized key")
	}
}

// principalFromPermissions builds the auth.Principal every
// AcquireSandboxHTTPClient call on this connection uses, from the
// PublicKeyCallback's recorded grant. A file-layer grant gets the full scope
// set (it authenticates as the server's own default user, same as the HTTP
// DefaultUserAuthenticator); a project-layer grant gets exactly the
// exec/tcp bundle ADR 0024 §5 describes, scoped to that project by virtue of
// every subsequent AcquireSandboxHTTPClient call being made with this
// connection's fixed projectID/sandboxID.
func principalFromPermissions(perms *ssh.Permissions, defaultUserID string) (principal auth.Principal, projectID, sandboxID string) {
	ext := map[string]string{}
	if perms != nil {
		ext = perms.Extensions
	}
	projectID = ext[projectExtension]
	sandboxID = ext[sandboxExtension]
	userID := ext[userIDExtension]
	if userID == "" {
		userID = defaultUserID
	}
	if ext[grantExtension] == grantFile {
		return auth.Principal{Type: auth.PrincipalTypeUser, UserID: userID, Scopes: []string{auth.ScopeAll}}, projectID, sandboxID
	}
	return auth.Principal{
		Type:   auth.PrincipalTypeUser,
		UserID: userID,
		Scopes: []string{poolagentauth.ScopeExecRead, poolagentauth.ScopeExecWrite, poolagentauth.ScopeTCPConnect},
	}, projectID, sandboxID
}
