package server

import (
	"context"
	"fmt"
	"log"
	"net"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/obot-platform/discobox/server/internal/config"
	"github.com/obot-platform/discobox/server/internal/service"
	"github.com/obot-platform/discobox/server/internal/services"
	"github.com/obot-platform/discobox/server/internal/sshd"
	"github.com/obot-platform/discobox/server/internal/store"
)

// resolveSSHIngress loads the host key and resolves the endpoint clients
// should dial, producing what GET /ssh serves. It runs before the router is
// built, because the discovery document is answered by an ordinary generated
// handler reading services.Services rather than by a hand-wired route bolted
// on after startup — and the host key is a file in the data directory, so
// reading it does not depend on the listener being up.
//
// SSH is always available: every server can serve it over the transport the API
// already answers on (`GET /ssh/connect`), so the host key is loaded
// unconditionally. DISCOBOX_SSH_LISTEN decides only whether there is *also* a
// TCP front door, which is the part that has to be opted into because it is a
// machine-wide surface. Those are two different questions, and Address answers
// the second: empty means "reachable, but not at an address you can put in a
// config file".
func resolveSSHIngress(cfg *config.Config) (services.SSHIngress, ssh.Signer, error) {
	hostKey, err := sshd.LoadOrCreateHostKey(cfg.DataDir)
	if err != nil {
		return services.SSHIngress{}, nil, fmt.Errorf("load SSH host key: %w", err)
	}
	return services.SSHIngress{
		Enabled: true,
		Address: cfg.SSHAdvertiseAddress,
		HostKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(hostKey.PublicKey()))),
	}, hostKey, nil
}

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
// newSSHServer builds the one sshd both front doors share: the optional TCP
// listener and the `GET /ssh/connect` route.
func newSSHServer(cfg *config.Config, hostKey ssh.Signer, appServices services.Services, appStore *store.Store) (*sshd.Server, error) {
	server, err := sshd.NewServer(sshd.Options{
		HostKey:       hostKey,
		DataDir:       cfg.DataDir,
		Store:         appStore,
		Sandboxes:     appServices.Sandboxes,
		DefaultUserID: service.DefaultUserID,
	})
	if err != nil {
		return nil, fmt.Errorf("initialize SSH server: %w", err)
	}
	return server, nil
}

func startSSHListener(ctx context.Context, cfg *config.Config, sshServer *sshd.Server) error {
	if cfg.SSHListen == "" {
		return nil
	}
	ln, err := new(net.ListenConfig).Listen(ctx, "tcp", cfg.SSHListen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.SSHListen, err)
	}
	log.Printf("listening on ssh://%s (advertised as %s)", cfg.SSHListen, cfg.SSHAdvertiseAddress)
	go func() {
		if err := sshServer.Serve(ctx, ln); err != nil && ctx.Err() == nil {
			log.Printf("ssh server: %v", err)
		}
	}()
	return nil
}
