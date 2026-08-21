package server

import (
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/discobox-ai/discobox/server/internal/config"
	"github.com/discobox-ai/discobox/server/internal/service"
	"github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/sshd"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// resolveSSHIngress loads the host key clients pin, producing what GET /ssh
// serves. It runs before the router is built, because the discovery document is
// answered by an ordinary generated handler reading services.Services rather
// than by a hand-wired route bolted on after startup — and the host key is a
// file in the data directory, so reading it does not depend on anything being
// listening.
//
// There is nothing else to discover. SSH reaches this server one way, over the
// transport the API already answers on (`GET /ssh/connect`), so every server
// serves it and none of them has an address of its own to advertise
// (ADR 0057).
func resolveSSHIngress(cfg *config.Config) (services.SSHIngress, ssh.Signer, error) {
	hostKey, err := sshd.LoadOrCreateHostKey(cfg.DataDir)
	if err != nil {
		return services.SSHIngress{}, nil, fmt.Errorf("load SSH host key: %w", err)
	}
	return services.SSHIngress{
		HostKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(hostKey.PublicKey()))),
	}, hostKey, nil
}

// newSSHServer builds the sshd behind `GET /ssh/connect`.
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
