package encryption

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	vault "github.com/hashicorp/vault/api"

	"github.com/discobox-ai/discobox/server/internal/config"
)

// serviceAccountToken is where Kubernetes mounts a pod's own token.
const serviceAccountToken = "/var/run/secrets/kubernetes.io/serviceaccount/token" //nolint:gosec // A path, not a credential.

// tokenFilePoll is how often a file: token is read again for a rotation.
const tokenFilePoll = 30 * time.Second

// newVaultClient logs in to Vault or OpenBao as cfg says. A kubernetes login
// is kept renewed, and repeated when it can no longer be; a token read from a
// file is read again, so one rotated outside the server is taken up. Either
// goes on until the returned stop is called or ctx ends.
func newVaultClient(ctx context.Context, cfg config.VaultTransitEncryptionSettings) (*vault.Client, func(), error) {
	vcfg := vault.DefaultConfig()
	if vcfg.Error != nil {
		return nil, nil, vcfg.Error
	}
	vcfg.Address = cfg.Address
	client, err := vault.NewClient(vcfg)
	if err != nil {
		return nil, nil, err
	}
	if cfg.Namespace != "" {
		client.SetNamespace(cfg.Namespace)
	}
	switch cfg.Auth {
	case config.VaultAuthKubernetes:
		secret, err := kubernetesLogin(ctx, client, cfg.Role)
		if err != nil {
			return nil, nil, err
		}
		renewCtx, cancel := context.WithCancel(ctx)
		go keepLoggedIn(renewCtx, client, cfg.Role, secret)
		return client, cancel, nil
	default:
		token, err := resolveFile(vaultTokenSetting, cfg.Token)
		if err != nil {
			return nil, nil, err
		}
		client.SetToken(token)
		if !strings.HasPrefix(strings.TrimSpace(cfg.Token), "file:") {
			return client, nil, nil
		}
		watchCtx, cancel := context.WithCancel(ctx)
		go followTokenFile(watchCtx, client, cfg.Token, token, tokenFilePoll)
		return client, cancel, nil
	}
}

const vaultTokenSetting = "encryption.vaulttransit.token"

// followTokenFile sets the client's token to the file's whenever it changes,
// reading it every interval, until ctx ends. A file that cannot be read keeps
// the token it last held.
func followTokenFile(ctx context.Context, client *vault.Client, setting, token string, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		next, err := resolveFile(vaultTokenSetting, setting)
		if err != nil {
			log.Printf("encryption: vault: %v; keeping the token last read", err)
			continue
		}
		if next != "" && next != token {
			client.SetToken(next)
			token = next
		}
	}
}

func kubernetesLogin(ctx context.Context, client *vault.Client, role string) (*vault.Secret, error) {
	jwt, err := os.ReadFile(serviceAccountToken)
	if err != nil {
		return nil, fmt.Errorf("kubernetes login: read the service-account token: %w", err)
	}
	secret, err := client.Logical().WriteWithContext(ctx, "auth/kubernetes/login", map[string]any{
		"role": role,
		"jwt":  string(jwt),
	})
	if err != nil {
		return nil, fmt.Errorf("kubernetes login as %s: %w", role, err)
	}
	if secret == nil || secret.Auth == nil || secret.Auth.ClientToken == "" {
		return nil, errors.New("kubernetes login returned no token")
	}
	client.SetToken(secret.Auth.ClientToken)
	return secret, nil
}

// keepLoggedIn renews the login in secret for as long as Vault allows, then
// logs in again, until ctx ends.
func keepLoggedIn(ctx context.Context, client *vault.Client, role string, secret *vault.Secret) {
	for ctx.Err() == nil {
		if secret != nil {
			if err := watchLogin(ctx, client, secret); err != nil {
				log.Printf("encryption: vault login renewal: %v", err)
			}
		}
		if ctx.Err() != nil {
			return
		}
		var err error
		if secret, err = kubernetesLogin(ctx, client, role); err != nil {
			log.Printf("encryption: vault: %v; retrying in 10s", err)
			select {
			case <-ctx.Done():
			case <-time.After(10 * time.Second):
			}
		}
	}
}

// watchLogin returns when the login in secret cannot be renewed further.
func watchLogin(ctx context.Context, client *vault.Client, secret *vault.Secret) error {
	watcher, err := client.NewLifetimeWatcher(&vault.LifetimeWatcherInput{Secret: secret})
	if err != nil {
		return err
	}
	go watcher.Start()
	defer watcher.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-watcher.DoneCh():
			return err
		case <-watcher.RenewCh():
		}
	}
}
