package server

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"

	"github.com/go-chi/chi/v5"
	"golang.org/x/crypto/ssh"

	"github.com/obot-platform/discobox/server/internal/config"
	"github.com/obot-platform/discobox/server/internal/service"
	"github.com/obot-platform/discobox/server/internal/services"
	"github.com/obot-platform/discobox/server/internal/sshd"
	"github.com/obot-platform/discobox/server/internal/store"
)

// startSSHListener starts the SSH control-plane ingress (ADR 0024) as an
// additional listener alongside the HTTP one when DISCOBOX_SSH_LISTEN is
// configured; it is a no-op otherwise, since a TCP listener is opted into,
// never implied (server/DESIGN.md "Listen Endpoints").
//
// It runs its own accept loop in a goroutine and is not folded into
// serveAll: HTTP listeners share one *http.Server whose Shutdown drains
// in-flight requests, but an SSH connection is not a request/response cycle
// to drain the same way — sshd.Server.Serve simply stops accepting and lets
// ctx cancellation close the listener, matching sshd's own connection
// lifecycle rather than http.Server's.
// startSSHListener returns the host key it loaded/generated so the caller can
// wire GET /ssh/host-key onto the HTTP router; it returns a nil signer,
// without error, when SSH is disabled.
func startSSHListener(ctx context.Context, cfg *config.Config, appServices services.Services, appStore *store.Store) (ssh.Signer, error) {
	if cfg.SSHListen == "" {
		return nil, nil
	}
	hostKey, err := sshd.LoadOrCreateHostKey(cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("load SSH host key: %w", err)
	}
	sshServer, err := sshd.NewServer(sshd.Options{
		HostKey:       hostKey,
		DataDir:       cfg.DataDir,
		Store:         appStore,
		Sandboxes:     appServices.Sandboxes,
		DefaultUserID: service.DefaultUserID,
	})
	if err != nil {
		return nil, fmt.Errorf("initialize SSH server: %w", err)
	}
	ln, err := new(net.ListenConfig).Listen(ctx, "tcp", cfg.SSHListen)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", cfg.SSHListen, err)
	}
	log.Printf("listening on ssh://%s", cfg.SSHListen)
	go func() {
		if err := sshServer.Serve(ctx, ln); err != nil && ctx.Err() == nil {
			log.Printf("ssh server: %v", err)
		}
	}()
	return hostKey, nil
}

// registerSSHHostKeyRoute serves the server's SSH host public key as a plain
// authorized_keys(5)-shaped line ("type base64"), the two fields
// `disco ssh-config` needs to build a known_hosts entry. It is a no-op when
// SSH is disabled (hostKey is nil).
func registerSSHHostKeyRoute(router chi.Router, hostKey ssh.Signer) {
	if hostKey == nil {
		return
	}
	line := ssh.MarshalAuthorizedKey(hostKey.PublicKey())
	router.Get("/ssh/host-key", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write(line)
	})
}
